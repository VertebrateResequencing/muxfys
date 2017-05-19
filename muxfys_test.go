// Copyright © 2017 Genome Research Limited
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
	"fmt"
	"github.com/inconshreveable/log15"
	. "github.com/smartystreets/goconvey/convey"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

var uploadFail bool

// localAccessor implements RemoteAccessor: it just accesses the local POSIX
// file system for testing purposes
type localAccessor struct {
	target string
}

func (a *localAccessor) copyFile(source, dest string) (err error) {
	in, err := os.Open(source)
	if err != nil {
		return
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return
	}
	defer func() {
		cerr := out.Close()
		if err == nil {
			err = cerr
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return
	}
	err = out.Sync()
	return
}

// DownloadFile implements RemoteAccessor by deferring to local fs.
func (a *localAccessor) DownloadFile(source, dest string) (err error) {
	return a.copyFile(source, dest)
}

// UploadFile implements RemoteAccessor by deferring to local fs.
func (a *localAccessor) UploadFile(source, dest, contentType string) error {
	if uploadFail {
		return fmt.Errorf("upload failed")
	}
	return a.copyFile(source, dest)
}

// ListEntries implements RemoteAccessor by deferring to local fs.
func (a *localAccessor) ListEntries(dir string) (ras []RemoteAttr, err error) {
	entries, err := ioutil.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		ras = append(ras, RemoteAttr{
			Name:  dir + name,
			Size:  entry.Size(),
			MTime: entry.ModTime(),
		})
	}
	return
}

// OpenFile implements RemoteAccessor by deferring to local fs.
func (a *localAccessor) OpenFile(path string) (io.ReadCloser, error) {
	return os.Open(path)
}

// Seek implements RemoteAccessor by deferring to local fs.
func (a *localAccessor) Seek(rc io.ReadCloser, offset int64) error {
	object := rc.(*os.File)
	_, err := object.Seek(offset, io.SeekStart)
	return err
}

// CopyFile implements RemoteAccessor by deferring to local fs.
func (a *localAccessor) CopyFile(source, dest string) error {
	return a.copyFile(source, dest)
}

// DeleteFile implements RemoteAccessor by deferring to local fs.
func (a *localAccessor) DeleteFile(path string) error {
	return os.Remove(path)
}

// ErrorIsNotExists implements RemoteAccessor by deferring to os.
func (a *localAccessor) ErrorIsNotExists(err error) bool {
	return os.IsNotExist(err)
}

// Target implements RemoteAccessor by returning the initial target we were
// configured with.
func (a *localAccessor) Target() string {
	return a.target
}

// RemotePath implements RemoteAccessor by using the initially configured target.
func (a *localAccessor) RemotePath(relPath string) string {
	return filepath.Join(a.target, relPath)
}

// LocalPath implements RemoteAccessor by adding nothing extra.
func (a *localAccessor) LocalPath(baseDir, remotePath string) string {
	return filepath.Join(baseDir, remotePath)
}

func TestMuxFys(t *testing.T) {
	tmpdir, err := ioutil.TempDir("", "muxfys_testing")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmpdir)
	err = os.Chdir(tmpdir)
	if err != nil {
		log.Fatal(err)
	}
	user, err := user.Current()
	if err != nil {
		log.Fatal(err)
	}

	sourcePoint := filepath.Join(tmpdir, "source")
	os.MkdirAll(sourcePoint, os.FileMode(0777))

	accessor := &localAccessor{
		target: sourcePoint,
	}

	// for testing purposes we override exitFunc and deathSignals
	var i int
	exitFunc = func(code int) {
		i = code
	}
	deathSignals = []os.Signal{syscall.SIGUNUSED}

	Convey("You can make a New MuxFys with an explicit Mount", t, func() {
		explicitMount := filepath.Join(tmpdir, "explicitMount")
		cfg := &Config{
			Mount:   explicitMount,
			Verbose: true,
		}
		fs, err := New(cfg)
		So(err, ShouldBeNil)

		Convey("You can Mount() read-only uncached", func() {
			remoteConfig := &RemoteConfig{
				Accessor:  accessor,
				CacheData: false,
				Write:     false,
			}
			err := fs.Mount(remoteConfig)
			So(err, ShouldBeNil)
			defer fs.Unmount()

			Convey("Once mounted you can't mount again", func() {
				err := fs.Mount(remoteConfig)
				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldEqual, "Can't mount more that once at a time")
			})

			Convey("You can Unmount()", func() {
				err := fs.Unmount()
				So(err, ShouldBeNil)
			})

			Convey("You can UnmountOnDeath()", func() {
				So(fs.handlingSignals, ShouldBeFalse)
				fs.UnmountOnDeath()
				So(fs.handlingSignals, ShouldBeTrue)
				So(fs.mounted, ShouldBeTrue)
				So(i, ShouldEqual, 0)

				// doing it again is harmless
				fs.UnmountOnDeath()

				syscall.Kill(syscall.Getpid(), syscall.SIGUNUSED)
				<-time.After(500 * time.Millisecond)

				So(fs.mounted, ShouldBeFalse)
				So(i, ShouldEqual, 1)
				i = 0
			})

			Convey("You can Unmount() while UnmountOnDeath() is active", func() {
				fs.UnmountOnDeath()
				So(fs.mounted, ShouldBeTrue)
				So(i, ShouldEqual, 0)

				err = fs.Unmount()
				So(err, ShouldBeNil)
				So(fs.mounted, ShouldBeFalse)

				syscall.Kill(syscall.Getpid(), syscall.SIGUNUSED)
				<-time.After(500 * time.Millisecond)

				So(i, ShouldEqual, 0)
			})
		})

		Convey("You can Mount() writable cached", func() {
			remoteConfig := &RemoteConfig{
				Accessor:  accessor,
				CacheData: true,
				Write:     true,
			}
			err := fs.Mount(remoteConfig)
			So(err, ShouldBeNil)
			defer fs.Unmount()

			Convey("You can Unmount()", func() {
				err := fs.Unmount()
				So(err, ShouldBeNil)
			})

			Convey("Unmounting after creating files uploads them", func() {
				sourceFile1 := filepath.Join(sourcePoint, "created1.file")
				_, err = os.Stat(sourceFile1)
				So(err, ShouldNotBeNil)
				sourceFile2 := filepath.Join(sourcePoint, "created2.file")
				_, err = os.Stat(sourceFile2)
				So(err, ShouldNotBeNil)

				f, err := os.OpenFile(filepath.Join(explicitMount, "created1.file"), os.O_RDWR|os.O_CREATE, 0666)
				So(err, ShouldBeNil)
				f.Close()
				defer os.Remove(sourceFile1)
				f, err = os.OpenFile(filepath.Join(explicitMount, "created2.file"), os.O_RDWR|os.O_CREATE, 0666)
				So(err, ShouldBeNil)
				f.Close()
				defer os.Remove(sourceFile2)

				err = fs.Unmount()
				So(err, ShouldBeNil)

				_, err = os.Stat(sourceFile1)
				So(err, ShouldBeNil)
				_, err = os.Stat(sourceFile2)
				So(err, ShouldBeNil)

				Convey("SetLogHandler() lets you log events", func() {
					buff := new(bytes.Buffer)
					SetLogHandler(log15.StreamHandler(buff, log15.LogfmtFormat()))

					err := fs.Mount(remoteConfig)
					So(err, ShouldBeNil)

					_, err = os.Stat(filepath.Join(explicitMount, "created1.file"))
					So(err, ShouldBeNil)

					logs := buff.String()
					So(logs, ShouldContainSubstring, "call=ListEntries")
				})
			})

			Convey("Unmounting reports failure to upload", func() {
				sourceFile := filepath.Join(sourcePoint, "created.file")
				_, err = os.Stat(sourceFile)
				So(err, ShouldNotBeNil)

				f, err := os.OpenFile(filepath.Join(explicitMount, "created.file"), os.O_RDWR|os.O_CREATE, 0666)
				So(err, ShouldBeNil)
				f.Close()

				uploadFail = true
				defer func() {
					uploadFail = false
				}()
				defer os.Remove(sourceFile)

				err = fs.Unmount()
				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldEqual, "failed to upload 1 files")

				Convey("Logs() tells you what happened", func() {
					logs := fs.Logs()
					So(len(logs), ShouldEqual, 3)
					So(logs[2], ShouldContainSubstring, "lvl=eror")
					So(logs[2], ShouldContainSubstring, `msg="Remote call failed"`)
					So(logs[2], ShouldContainSubstring, "pkg=muxfys")
					So(logs[2], ShouldContainSubstring, "mount="+explicitMount)
					So(logs[2], ShouldContainSubstring, "target="+sourcePoint)
					So(logs[2], ShouldContainSubstring, "call=UploadFile")
					So(logs[2], ShouldContainSubstring, "path="+sourceFile)
					So(logs[2], ShouldContainSubstring, "retries=0")
					So(logs[2], ShouldContainSubstring, "walltime=")
					So(logs[2], ShouldContainSubstring, `err="upload failed"`)
					So(logs[2], ShouldContainSubstring, "caller=remote.go")
				})
			})

			Convey("You can't have 2 writeable remotes", func() {
				err := fs.Unmount()
				So(err, ShouldBeNil)
				err = fs.Mount(remoteConfig, remoteConfig)
				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldEqual, "You can't have more than one writeable remote")
			})

			Convey("UnmountOnDeath() will exit(2) on failure to unmount", func() {
				fs.UnmountOnDeath()
				So(fs.mounted, ShouldBeTrue)
				So(i, ShouldEqual, 0)

				f, err := os.OpenFile(filepath.Join(explicitMount, "opened.file"), os.O_RDWR|os.O_CREATE, 0666)
				So(err, ShouldBeNil)

				syscall.Kill(syscall.Getpid(), syscall.SIGUNUSED)
				<-time.After(500 * time.Millisecond)

				So(fs.mounted, ShouldBeTrue)
				So(i, ShouldEqual, 2)
				i = 0

				f.Close()
				err = fs.Unmount()
				So(err, ShouldBeNil)
				So(fs.mounted, ShouldBeFalse)
			})
		})

		Convey("You must supply at least one RemoteConfig to Mount()", func() {
			err := fs.Mount()
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldEqual, "At least one RemoteConfig must be supplied")
		})

		Convey("You can't Mount() with a bad CacheDir", func() {
			remoteConfig := &RemoteConfig{
				Accessor: accessor,
				CacheDir: "/!",
			}
			err := fs.Mount(remoteConfig)
			So(err, ShouldNotBeNil)
		})

		Convey("UnmountOnDeath does nothing prior to mounting", func() {
			So(fs.handlingSignals, ShouldBeFalse)
			fs.UnmountOnDeath()
			So(fs.handlingSignals, ShouldBeFalse)
		})
	})

	Convey("You can make a New MuxFys with a default Mount", t, func() {
		defaultMnt := filepath.Join(tmpdir, "mnt")
		fs, err := New(&Config{})
		So(err, ShouldBeNil)
		So(fs.mountPoint, ShouldEqual, defaultMnt)
		_, err = os.Stat(defaultMnt)
		So(err, ShouldBeNil)
	})

	Convey("You can make a New MuxFys with an explicit ~ Mount", t, func() {
		expectedMount := filepath.Join(user.HomeDir, ".muxfys_test_mount_dir")
		explicitMount := "~/.muxfys_test_mount_dir"
		cfg := &Config{
			Mount: explicitMount,
		}
		fs, err := New(cfg)
		defer os.RemoveAll(expectedMount)
		So(err, ShouldBeNil)
		So(fs.mountPoint, ShouldEqual, expectedMount)
		_, err = os.Stat(expectedMount)
		So(err, ShouldBeNil)

		Convey("This fails for invalid home dir specs", func() {
			explicitMount := "~.muxfys_test_mount_dir"
			cfg := &Config{
				Mount: explicitMount,
			}
			_, err := New(cfg)
			So(err, ShouldNotBeNil)
		})
	})

	if user.Name != "root" {
		Convey("You can't make a New MuxFys with Mount point in /", t, func() {
			explicitMount := "/.muxfys_test_mount_dir"
			cfg := &Config{
				Mount: explicitMount,
			}
			_, err := New(cfg)
			defer os.RemoveAll(explicitMount)
			So(err, ShouldNotBeNil)
		})
	}

	Convey("You can't make a New MuxFys using a file as a Mount", t, func() {
		explicitMount := filepath.Join(tmpdir, "mntfile")
		os.OpenFile(explicitMount, os.O_RDONLY|os.O_CREATE, 0666)
		cfg := &Config{
			Mount: explicitMount,
		}
		_, err := New(cfg)
		defer os.RemoveAll(explicitMount)
		So(err, ShouldNotBeNil)
	})

	Convey("You can't make a New MuxFys using a Mount that already contains files", t, func() {
		explicitMount := filepath.Join(tmpdir, "mntfull")
		err := os.MkdirAll(explicitMount, os.FileMode(0777))
		So(err, ShouldBeNil)
		os.OpenFile(filepath.Join(explicitMount, "mntfile"), os.O_RDONLY|os.O_CREATE, 0666)
		cfg := &Config{
			Mount: explicitMount,
		}
		_, err = New(cfg)
		defer os.RemoveAll(explicitMount)
		So(err, ShouldNotBeNil)
		So(err.Error(), ShouldContainSubstring, "was not empty")
	})
}
