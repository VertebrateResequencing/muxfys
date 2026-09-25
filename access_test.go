// Copyright © 2026 Genome Research Limited
// Author: Sendu Bala <sb10@sanger.ac.uk>.
//
//  This file is part of muxfys.
//
//  muxfys is free software: you can redistribute it and/or modify
//  it under the terms of the GNU Lesser General Public License as published by
//  the Free Software Foundation, either version 3 of the License, or
//  (at your option) any later version.
//
//  muxfys is distributed in the hope that it will be useful,
//  but WITHOUT ANY WARRANTY; without even the implied warranty of
//  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
//  GNU Lesser General Public License for more details.
//
//  You should have received a copy of the GNU Lesser General Public License
//  along with muxfys. If not, see <http://www.gnu.org/licenses/>.

package muxfys

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
	"golang.org/x/sys/unix"
)

const (
	forkChildEnv    = "MUXFYS_TEST_FORK_DIR"
	forkTrials      = 3
	forkDuration    = time.Second
	forkStall       = 2 * time.Second
	forkReapTimeout = 10 * time.Second
	forkPoll        = 50 * time.Millisecond
	forkDoneMsg     = "done"
	fuseFsType      = "fuse.MuxFys"
	fusectlPrefix   = "/sys/fs/fuse/connections/"
)

var errForkChild = errors.New("fork child failed")

// TestMountForkChdir runs, in a re-exec'd copy of this test binary, a process
// that mounts muxfys and then repeatedly execs a child whose working directory
// is the mount root while garbage collecting back-to-back. Go forks with
// CLONE_VFORK, so if the child's chdir needs a FUSE request answered while a
// stop-the-world GC is waiting on the vforking thread, the process wedges.
func TestMountForkChdir(t *testing.T) {
	if dir := os.Getenv(forkChildEnv); dir != "" {
		if err := forkChild(t.Context(), dir); err != nil {
			t.Fatal(err)
		}

		return
	}

	fusermount := skipWithoutFuse(t)

	Convey("A process that mounts muxfys can exec children with their cwd on the mount root", t, func() {
		wedged := 0

		for trial := range forkTrials {
			if forkTrialWedged(t, fusermount) {
				t.Logf("trial %d wedged", trial+1)

				wedged++
			}
		}

		So(wedged, ShouldEqual, 0)
	})
}

// forkChild is the re-exec'd side of TestMountForkChdir. It mounts dir/mnt,
// then execs /bin/true with cmd.Dir on the mount root for forkDuration while
// garbage collecting, writing a line to stdout before every exec so that the
// parent can see progress.
func forkChild(ctx context.Context, dir string) error {
	mnt := filepath.Join(dir, "mnt")

	fs, err := New(&Config{Mount: mnt, CacheBase: dir})
	if err != nil {
		return err
	}

	if err = fs.Mount(&RemoteConfig{Accessor: &localAccessor{target: filepath.Join(dir, "src")}}); err != nil {
		return err
	}

	var stop atomic.Bool

	go func() {
		for !stop.Load() {
			runtime.GC()
		}
	}()

	err = execLoop(ctx, mnt)

	stop.Store(true)

	return errors.Join(err, fs.Unmount())
}

func execLoop(ctx context.Context, mnt string) error {
	end := time.Now().Add(forkDuration)

	for i := 0; time.Now().Before(end); i++ {
		fmt.Printf("exec %d\n", i) //nolint:forbidigo

		cmd := exec.CommandContext(ctx, "/bin/true")
		cmd.Dir = mnt

		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%w: exec %d: %w", errForkChild, i, err)
		}
	}

	fmt.Println(forkDoneMsg) //nolint:forbidigo

	return nil
}

func TestMountAccess(t *testing.T) {
	skipWithoutFuse(t)

	Convey("Given a writable mount", t, func() {
		dir, err := filepath.EvalSymlinks(t.TempDir())
		So(err, ShouldBeNil)

		src := filepath.Join(dir, "src")
		So(os.Mkdir(src, dirMode), ShouldBeNil)
		So(os.WriteFile(filepath.Join(src, "read.file"), []byte("a\n"), fileMode), ShouldBeNil)

		mnt := filepath.Join(dir, "mnt")
		fs, err := New(&Config{Mount: mnt, CacheBase: dir})
		So(err, ShouldBeNil)
		So(fs.Mount(&RemoteConfig{Accessor: &localAccessor{target: src}, CacheData: true, Write: true}), ShouldBeNil)

		defer func() { So(fs.Unmount(), ShouldBeNil) }()

		created := filepath.Join(mnt, "created.file")
		So(os.WriteFile(created, []byte("b\n"), fileMode), ShouldBeNil)

		Convey("access(2) grants every mode on its directories and files", func() {
			for _, path := range []string{mnt, filepath.Join(mnt, "read.file"), created} {
				So(unix.Access(path, unix.R_OK|unix.W_OK|unix.X_OK), ShouldBeNil)
			}
		})

		Convey("test -r, -w and -x succeed on its root", func() {
			for _, flag := range []string{"-r", "-w", "-x"} {
				var stderr bytes.Buffer

				cmd := exec.CommandContext(t.Context(), "test", flag, mnt)
				cmd.Stderr = &stderr

				So(cmd.Run(), ShouldBeNil)
				So(stderr.String(), ShouldBeEmpty)
			}
		})

		Convey("test -w succeeds on files in it", func() {
			So(exec.CommandContext(t.Context(), "test", "-w", created).Run(), ShouldBeNil)
			So(exec.CommandContext(t.Context(), "test", "-w", filepath.Join(mnt, "read.file")).Run(), ShouldBeNil)
		})
	})
}

// skipWithoutFuse skips the test if FUSE can't be used here, otherwise
// returning the path to fusermount.
func skipWithoutFuse(t *testing.T) string {
	t.Helper()

	f, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("/dev/fuse not usable: %s", err)
	}

	if err = f.Close(); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"fusermount3", "fusermount"} {
		if path, errl := exec.LookPath(name); errl == nil {
			return path
		}
	}

	t.Skip("fusermount not found")

	return ""
}

// forkTrialWedged runs forkChild in a new process and returns true if it
// stopped making progress. A wedged child is killed, its own FUSE connection
// aborted and its mount lazily unmounted.
func forkTrialWedged(t *testing.T, fusermount string) bool {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	So(err, ShouldBeNil)
	So(os.Mkdir(filepath.Join(dir, "src"), dirMode), ShouldBeNil)

	out := filepath.Join(dir, "out")
	cmd, done := startForkChild(t, dir, out)

	if !waitForForkChild(done, out) {
		So(<-done, ShouldBeNil)
		So(readFile(out), ShouldContainSubstring, forkDoneMsg+"\n")

		return false
	}

	mnt := filepath.Join(dir, "mnt")
	conn := fuseConnection(mnt)

	So(syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL), ShouldBeNil)

	if conn != "" && fuseConnection(mnt) == conn {
		So(os.WriteFile(fusectlPrefix+conn+"/abort", []byte("1"), 0), ShouldBeNil)
	}

	reaped := false

	select {
	case <-done:
		reaped = true
	case <-time.After(forkReapTimeout):
	}

	So(exec.CommandContext(t.Context(), fusermount, "-u", "-z", mnt).Run(), ShouldBeNil)
	So(reaped, ShouldBeTrue)

	return true
}

func startForkChild(t *testing.T, dir, out string) (*exec.Cmd, chan error) {
	t.Helper()

	f, err := os.Create(out)
	So(err, ShouldBeNil)

	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestMountForkChdir$")

	cmd.Env = append(os.Environ(), forkChildEnv+"="+dir)
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	So(cmd.Start(), ShouldBeNil)
	So(f.Close(), ShouldBeNil)

	done := make(chan error, 1)

	go func() { done <- cmd.Wait() }()

	return cmd, done
}

// waitForForkChild waits for the child to exit, returning true if instead its
// output stopped growing for forkStall. If false, the child's exit error is
// still in done.
func waitForForkChild(done chan error, out string) bool {
	size, since := int64(-1), time.Now()

	for {
		select {
		case err := <-done:
			done <- err

			return false
		case <-time.After(forkPoll):
		}

		if fi, err := os.Stat(out); err == nil && fi.Size() != size {
			size, since = fi.Size(), time.Now()
		} else if time.Since(since) > forkStall {
			return true
		}
	}
}

// fuseConnection returns the fusectl connection of the muxfys mount at mnt:
// the minor of its st_dev in our mountinfo, or "" if mnt isn't such a mount.
func fuseConnection(mnt string) string {
	for line := range strings.Lines(readFile("/proc/self/mountinfo")) {
		before, after, found := strings.Cut(line, " - ")
		fields, fsFields := strings.Fields(before), strings.Fields(after)

		if !found || len(fields) < 5 || len(fsFields) < 1 || fields[4] != mnt || fsFields[0] != fuseFsType {
			continue
		}

		if minor, isAnon := strings.CutPrefix(fields[2], "0:"); isAnon {
			return minor
		}
	}

	return ""
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	So(err, ShouldBeNil)

	return string(b)
}
