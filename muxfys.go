// Copyright © 2017 Genome Research Limited
// Author: Sendu Bala <sb10@sanger.ac.uk>.
// The target parsing code in this file is based on code in
// https://github.com/minio/minfs Copyright 2016 Minio, Inc.
// licensed under the Apache License, Version 2.0 (the "License"), stating:
// "You may not use this file except in compliance with the License. You may
// obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0"
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

/*
Package muxfys is a pure Go library that lets you in-process temporarily
fuse-mount remote file systems or object stores as a "filey" system. Currently
only support for S3-like systems has been implemented.

It has high performance, and is easy to use with nothing else to install, and no
root permissions needed (except to initially install/configure fuse: on old
linux you may need to install fuse-utils, and for macOS you'll need to install
osxfuse; for both you must ensure that 'user_allow_other' is set in
/etc/fuse.conf or equivalent).

It allows "multiplexing": you can mount multiple different buckets (or sub
directories of the same bucket) on the same local directory. This makes commands
you want to run against the files in your buckets much simpler, eg. instead of
mounting s3://publicbucket, s3://myinputbucket and s3://myoutputbucket to
separate mount points and running:
$ myexe -ref /mnt/publicbucket/refs/human/ref.fa -i /mnt/myinputbucket/xyz/123/
  input.file > /mnt/myoutputbucket/xyz/123/output.file
You could multiplex the 3 buckets (at the desired paths) on to the directory you
will work from and just run:
$ myexe -ref ref.fa -i input.file > output.file

When using muxfys, you 1) mount, 2) do something that needs the files in your S3
bucket(s), 3) unmount. Then repeat 1-3 for other things that need data in your
S3 buckets.

# Usage

    import "github.com/VertebrateResequencing/wr/muxfys"

    // fully manual target configuration
    target1 := &muxfys.Target{
        Target:     "https://s3.amazonaws.com/mybucket/subdir",
        Region:     "us-east-1",
        AccessKey:  os.Getenv("AWS_ACCESS_KEY_ID"),
        SecretKey:  os.Getenv("AWS_SECRET_ACCESS_KEY"),
        CacheDir:   "/tmp/muxfys/cache",
        Write:      true,
    }

    // or read some configuration from standard AWS S3 config files and
    // environment variables
    target2 := &muxfys.Target{
        CacheData: true,
    }
    target2.ReadEnvironment("default", "myotherbucket/another/subdir")

    cfg := &muxfys.Config{
        Mount: "/tmp/muxfys/mount",
        CacheBase: "/tmp",
        Retries:    3,
        Verbose:    true,
        Targets:    []*muxfys.Target{target, target2},
    }

    fs, err := muxfys.New(cfg)
    if err != nil {
        log.Fatalf("bad configuration: %s\n", err)
    }

    err = fs.Mount()
    if err != nil {
        log.Fatalf("could not mount: %s\n", err)
    }
    fs.UnmountOnDeath()

    // read from & write to files in /tmp/muxfys/mount, which contains the
    // contents of mybucket/subdir and myotherbucket/another/subdir; writes will
    // get uploaded to mybucket/subdir when you Unmount()

    err = fs.Unmount()
    if err != nil {
        log.Fatalf("could not unmount: %s\n", err)
    }

    logs := fs.Logs()
*/
package muxfys

import (
	"bufio"
	"fmt"
	"github.com/go-ini/ini"
	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/fuse/nodefs"
	"github.com/hanwen/go-fuse/fuse/pathfs"
	"github.com/inconshreveable/log15"
	"github.com/jpillora/backoff"
	"github.com/minio/minio-go"
	"github.com/mitchellh/go-homedir"
	"github.com/sb10/l15h"
	"io/ioutil"
	"net/url"
	"os"
	"os/signal"
	"os/user"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultDomain = "s3.amazonaws.com"
	dirMode       = 0700
	fileMode      = 0600
	dirSize       = uint64(4096)
	symlinkSize   = uint64(7)
)

var (
	logHandlerSetter = l15h.NewChanger(log15.DiscardHandler())
	pkgLogger        = log15.New("pkg", "muxfys")
)

func init() {
	pkgLogger.SetHandler(l15h.ChangeableHandler(logHandlerSetter))
}

// Config struct provides the configuration of a MuxFys.
type Config struct {
	// Mount is the local directory to mount on top of (muxfys will try to
	// create this if it doesn't exist). If not supplied, defaults to the
	// subdirectory "mnt" in the current working directory. Note that mounting
	// will only succeed if the Mount directory either doesn't exist or is
	// empty.
	Mount string

	// Retries is the number of times to automatically retry failed remote S3
	// system requests. The default of 0 means don't retry; at least 3 is
	// recommended.
	Retries int

	// CacheBase is the base directory that will be used to create any Target
	// cache directories, when those Targets have CacheData true but CacheDir
	// undefined. Defaults to the current working directory.
	CacheBase string

	// Verbose results in every remote request getting an entry in the output of
	// Logs(). Errors always appear there.
	Verbose bool

	// Targets is a slice of Target, describing what you want to mount and
	// allowing you to multiplex more than one bucket/ sub directory on to
	// Mount. Only 1 of these Target can be writeable.
	Targets []*Target
}

// Target struct provides details of the remote target (S3 bucket) you wish to
// mount, and particulars about caching and writing for this target.
type Target struct {
	// The full URL of your bucket and possible sub-path, eg.
	// https://cog.domain.com/bucket/subpath. For performance reasons, you
	// should specify the deepest subpath that holds all your files. This will
	// be set for you by a call to ReadEnvironment().
	Target string

	// Region is optional if you need to use a specific region. This can be set
	// for you by a call to ReadEnvironment().
	Region string

	// AccessKey and SecretKey can be set for you by calling ReadEnvironment().
	AccessKey string
	SecretKey string

	// CacheData enables caching of remote files that you read locally on disk.
	// Writes will also be staged on local disk prior to upload.
	CacheData bool

	// CacheDir is the directory used to cache data if CacheData is true.
	// (muxfys will try to create this if it doesn't exist). If not supplied
	// when CacheData is true, muxfys will create a unique temporary directory
	// in the CacheBase directory of the containing Config (these get
	// automatically deleted on Unmount() - specified CacheDirs do not).
	// Defining this makes CacheData be treated as true.
	CacheDir string

	// Write enables write operations in the mount. Only set true if you know
	// you really need to write. Since writing currently requires caching of
	// data, CacheData will be treated as true.
	Write bool
}

// ReadEnvironment sets Target, AccessKey and SecretKey and possibly Region. It
// determines these by looking primarily at the given profile section of
// ~/.s3cfg (s3cmd's config file). If profile is an empty string, it comes from
// $AWS_DEFAULT_PROFILE or $AWS_PROFILE or defaults to "default". If ~/.s3cfg
// doesn't exist or isn't fully specified, missing values will be taken from the
// file pointed to by $AWS_SHARED_CREDENTIALS_FILE, or ~/.aws/credentials (in
// the AWS CLI format) if that is not set. If this file also doesn't exist,
// ~/.awssecret (in the format used by s3fs) is used instead. AccessKey and
// SecretKey values will always preferably come from $AWS_ACCESS_KEY_ID and
// $AWS_SECRET_ACCESS_KEY respectively, if those are set. If no config file
// specified host_base, the default domain used is s3.amazonaws.com. Region is
// set by the $AWS_DEFAULT_REGION environment variable, or if that is not set,
// by checking the file pointed to by $AWS_CONFIG_FILE (~/.aws/config if unset).
// To allow the use of a single configuration file, users can create a non-
// standard file that specifies all relevant options: use_https, host_base,
// region, access_key (or aws_access_key_id) and secret_key (or
// aws_secret_access_key) (saved in any of the files except ~/.awssecret). The
// path argument should at least be the bucket name, but ideally should also
// specify the deepest subpath that holds all the files that need to be
// accessed. Because reading from a public s3.amazonaws.com bucket requires no
// credentials, no error is raised on failure to find any values in the
// environment when profile is supplied as an empty string.
func (t *Target) ReadEnvironment(profile, path string) error {
	if path == "" {
		return fmt.Errorf("muxfys ReadEnvironment() requires a path")
	}

	profileSpecified := true
	if profile == "" {
		if profile = os.Getenv("AWS_DEFAULT_PROFILE"); profile == "" {
			if profile = os.Getenv("AWS_PROFILE"); profile == "" {
				profile = "default"
				profileSpecified = false
			}
		}
	}

	s3cfg, err := homedir.Expand("~/.s3cfg")
	if err != nil {
		return err
	}
	ascf, err := homedir.Expand(os.Getenv("AWS_SHARED_CREDENTIALS_FILE"))
	if err != nil {
		return err
	}
	acred, err := homedir.Expand("~/.aws/credentials")
	if err != nil {
		return err
	}
	aconf, err := homedir.Expand(os.Getenv("AWS_CONFIG_FILE"))
	if err != nil {
		return err
	}
	acon, err := homedir.Expand("~/.aws/config")
	if err != nil {
		return err
	}

	aws, err := ini.LooseLoad(s3cfg, ascf, acred, aconf, acon)
	if err != nil {
		return fmt.Errorf("muxfys ReadEnvironment() loose loading of config files failed: %s", err)
	}

	var domain, key, secret, region string
	var https bool
	section, err := aws.GetSection(profile)
	if err == nil {
		https = section.Key("use_https").MustBool(false)
		domain = section.Key("host_base").String()
		region = section.Key("region").String()
		key = section.Key("access_key").MustString(section.Key("aws_access_key_id").MustString(os.Getenv("AWS_ACCESS_KEY_ID")))
		secret = section.Key("secret_key").MustString(section.Key("aws_secret_access_key").MustString(os.Getenv("AWS_SECRET_ACCESS_KEY")))
	} else if profileSpecified {
		return fmt.Errorf("muxfys ReadEnvironment(%s) called, but no config files defined that profile", profile)
	}

	if key == "" && secret == "" {
		// last resort, check ~/.awssecret
		awsSec, err := homedir.Expand("~/.awssecret")
		if err != nil {
			return err
		}
		if file, err := os.Open(awsSec); err == nil {
			defer file.Close()

			scanner := bufio.NewScanner(file)
			if scanner.Scan() {
				line := scanner.Text()
				if line != "" {
					line = strings.TrimSuffix(line, "\n")
					ks := strings.Split(line, ":")
					if len(ks) == 2 {
						key = ks[0]
						secret = ks[1]
					}
				}
			}
		}
	}

	if os.Getenv("AWS_ACCESS_KEY_ID") != "" {
		key = os.Getenv("AWS_ACCESS_KEY_ID")
	}
	if os.Getenv("AWS_SECRET_ACCESS_KEY") != "" {
		secret = os.Getenv("AWS_SECRET_ACCESS_KEY")
	}
	t.AccessKey = key
	t.SecretKey = secret

	if domain == "" {
		domain = defaultDomain
	}

	scheme := "http"
	if https {
		scheme += "s"
	}
	u := &url.URL{
		Scheme: scheme,
		Host:   domain,
		Path:   path,
	}
	t.Target = u.String()

	if os.Getenv("AWS_DEFAULT_REGION") != "" {
		t.Region = os.Getenv("AWS_DEFAULT_REGION")
	} else if region != "" {
		t.Region = region
	}

	return nil
}

// CreateRemote uses the configured details of the Target to create a *remote,
// used internally by MuxFys.New().
func (t *Target) CreateRemote(cacheBase string, maxAttempts int, logger log15.Logger) (r *remote, err error) {
	// parse the target to get secure, host, bucket and basePath
	if t.Target == "" {
		return nil, fmt.Errorf("no Target defined")
	}

	u, err := url.Parse(t.Target)
	if err != nil {
		return
	}

	var secure bool
	if strings.HasPrefix(t.Target, "https") {
		secure = true
	}

	host := u.Host
	var bucket, basePath string
	if len(u.Path) > 1 {
		parts := strings.Split(u.Path[1:], "/")
		if len(parts) >= 0 {
			bucket = parts[0]
		}
		if len(parts) >= 1 {
			basePath = path.Join(parts[1:]...)
		}
	}

	if bucket == "" {
		return nil, fmt.Errorf("no bucket could be determined from [%s]", t.Target)
	}

	// handle CacheData option, creating cache dir if necessary
	var cacheData bool
	if t.CacheData || t.CacheDir != "" || t.Write {
		cacheData = true
	}

	cacheDir := t.CacheDir
	if cacheDir != "" {
		cacheDir, err = homedir.Expand(cacheDir)
		if err != nil {
			return
		}
		cacheDir, err = filepath.Abs(cacheDir)
		if err != nil {
			return
		}
		err = os.MkdirAll(cacheDir, os.FileMode(dirMode))
		if err != nil {
			return
		}
	}

	deleteCache := false
	if cacheData && cacheDir == "" {
		// decide on our own cache directory
		cacheDir, err = ioutil.TempDir(cacheBase, ".muxfys_cache")
		if err != nil {
			return
		}
		deleteCache = true
	}

	r = &remote{
		CacheTracker: NewCacheTracker(),
		host:         host,
		bucket:       bucket,
		basePath:     basePath,
		cacheData:    cacheData,
		cacheDir:     cacheDir,
		cacheIsTmp:   deleteCache,
		write:        t.Write,
		maxAttempts:  maxAttempts,
		Logger:       logger.New("target", t.Target),
	}

	r.clientBackoff = &backoff.Backoff{
		Min:    100 * time.Millisecond,
		Max:    10 * time.Second,
		Factor: 3,
		Jitter: true,
	}

	// create a client for interacting with S3 (we do this here instead of
	// as-needed inside remote because there's large overhead in creating these)
	if t.Region != "" {
		r.client, err = minio.NewWithRegion(host, t.AccessKey, t.SecretKey, secure, t.Region)
	} else {
		r.client, err = minio.New(host, t.AccessKey, t.SecretKey, secure)
	}
	return
}

// MuxFys struct is the main filey system object.
type MuxFys struct {
	pathfs.FileSystem
	mountPoint      string
	dirAttr         *fuse.Attr
	server          *fuse.Server
	mutex           sync.Mutex
	dirs            map[string][]*remote
	dirContents     map[string][]fuse.DirEntry
	files           map[string]*fuse.Attr
	fileToRemote    map[string]*remote
	createdFiles    map[string]bool
	createdDirs     map[string]bool
	mounted         bool
	handlingSignals bool
	deathSignals    chan os.Signal
	ignoreSignals   chan bool
	remotes         []*remote
	writeRemote     *remote
	logStore        *l15h.Store
	log15.Logger
}

// New returns a MuxFys that you'll use to Mount() your S3 bucket(s), ensure you
// un-mount if killed by calling UnmountOnDeath(), then Unmount() when you're
// done. You might check Logs() afterwards. The other methods of MuxFys can be
// ignored in most cases.
func New(config *Config) (fs *MuxFys, err error) {
	if len(config.Targets) == 0 {
		return nil, fmt.Errorf("no targets provided")
	}

	mountPoint := config.Mount
	if mountPoint == "" {
		mountPoint = "mnt"
	}
	mountPoint, err = homedir.Expand(mountPoint)
	if err != nil {
		return
	}
	mountPoint, err = filepath.Abs(mountPoint)
	if err != nil {
		return
	}

	// create mount point if necessary
	err = os.MkdirAll(mountPoint, os.FileMode(dirMode))
	if err != nil {
		return
	}

	// check that it's empty
	entries, err := ioutil.ReadDir(mountPoint)
	if err != nil {
		return
	}
	if len(entries) > 0 {
		return nil, fmt.Errorf("Mount directory %s was not empty", mountPoint)
	}

	cacheBase := config.CacheBase
	if cacheBase == "" {
		cacheBase, err = os.Getwd()
		if err != nil {
			return
		}
	}

	// make a logger with context for us, that will store log messages in memory
	// but is also capable of logging anywhere the user wants via
	// SetLogHandler()
	logger := pkgLogger.New("mount", mountPoint)
	store := l15h.NewStore()
	logLevel := log15.LvlError
	if config.Verbose {
		logLevel = log15.LvlInfo
	}
	l15h.AddHandler(logger, log15.LvlFilterHandler(logLevel, l15h.CallerInfoHandler(l15h.StoreHandler(store, log15.LogfmtFormat()))))

	// initialize ourselves
	fs = &MuxFys{
		FileSystem:   pathfs.NewDefaultFileSystem(),
		mountPoint:   mountPoint,
		dirs:         make(map[string][]*remote),
		dirContents:  make(map[string][]fuse.DirEntry),
		files:        make(map[string]*fuse.Attr),
		fileToRemote: make(map[string]*remote),
		createdFiles: make(map[string]bool),
		createdDirs:  make(map[string]bool),
		logStore:     store,
		Logger:       logger,
	}

	// create a remote for every Target
	for _, t := range config.Targets {
		var r *remote
		r, err = t.CreateRemote(cacheBase, config.Retries+1, logger)
		if err != nil {
			return
		}

		fs.remotes = append(fs.remotes, r)
		if r.write {
			if fs.writeRemote != nil {
				return nil, fmt.Errorf("you can't have more than one writeable target")
			}
			fs.writeRemote = r
		}
	}

	// cheats for s3-like filesystems
	mTime := uint64(time.Now().Unix())
	fs.dirAttr = &fuse.Attr{
		Size:  dirSize,
		Mode:  fuse.S_IFDIR | uint32(dirMode),
		Mtime: mTime,
		Atime: mTime,
		Ctime: mTime,
	}

	return
}

// Mount carries out the mounting of your configured S3 bucket to your
// configured mount point. On return, the files in your bucket will be
// accessible. Once mounted, you can't mount again until you Unmount().
func (fs *MuxFys) Mount() (err error) {
	fs.mutex.Lock()
	defer fs.mutex.Unlock()
	if fs.mounted {
		err = fmt.Errorf("Can't mount more that once at a time\n")
		return
	}

	uid, gid, err := userAndGroup()
	if err != nil {
		return
	}

	opts := &nodefs.Options{
		NegativeTimeout: time.Second,
		AttrTimeout:     time.Second,
		EntryTimeout:    time.Second,
		Owner: &fuse.Owner{
			Uid: uid,
			Gid: gid,
		},
		Debug: false,
	}
	pathFsOpts := &pathfs.PathNodeFsOptions{ClientInodes: false}
	pathFs := pathfs.NewPathNodeFs(fs, pathFsOpts)
	conn := nodefs.NewFileSystemConnector(pathFs.Root(), opts)
	mOpts := &fuse.MountOptions{
		AllowOther:           true,
		FsName:               "MuxFys",
		Name:                 "MuxFys",
		RememberInodes:       true,
		DisableXAttrs:        true,
		IgnoreSecurityLabels: true,
		Debug:                false,
	}
	server, err := fuse.NewServer(conn.RawFS(), fs.mountPoint, mOpts)
	if err != nil {
		return
	}

	fs.server = server
	go server.Serve()

	fs.mounted = true
	return
}

// userAndGroup returns the current uid and gid; we only ever mount with dir and
// file permissions for the current user.
func userAndGroup() (uid uint32, gid uint32, err error) {
	user, err := user.Current()
	if err != nil {
		return
	}

	uid64, err := strconv.ParseInt(user.Uid, 10, 32)
	if err != nil {
		return
	}

	gid64, err := strconv.ParseInt(user.Gid, 10, 32)
	if err != nil {
		return
	}

	uid = uint32(uid64)
	gid = uint32(gid64)

	return
}

// UnmountOnDeath captures SIGINT (ctrl-c) and SIGTERM (kill) signals, then
// calls Unmount() before calling os.Exit(1 if the unmount worked, 2 otherwise)
// to terminate your program. Manually calling Unmount() after this cancels the
// signal capture. This does NOT block.
func (fs *MuxFys) UnmountOnDeath() {
	fs.mutex.Lock()
	defer fs.mutex.Unlock()
	if !fs.mounted || fs.handlingSignals {
		return
	}

	fs.deathSignals = make(chan os.Signal, 2)
	signal.Notify(fs.deathSignals, os.Interrupt, syscall.SIGTERM)
	fs.handlingSignals = true
	fs.ignoreSignals = make(chan bool)

	go func() {
		select {
		case <-fs.ignoreSignals:
			signal.Stop(fs.deathSignals)
			fs.mutex.Lock()
			fs.handlingSignals = false
			fs.mutex.Unlock()
			return
		case <-fs.deathSignals:
			fs.mutex.Lock()
			fs.handlingSignals = false
			fs.mutex.Unlock()
			err := fs.Unmount()
			if err != nil {
				fs.Error("Failed to unmount on death", "err", err)
				os.Exit(2)
			}
			os.Exit(1)
		}
	}()
}

// Unmount must be called when you're done reading from/ writing to your bucket.
// Be sure to close any open filehandles before hand! It's a good idea to defer
// this after calling Mount(), and possibly also call UnmountOnDeath(). In
// CacheData mode, it is only at Unmount() that any files you created or altered
// get uploaded, so this may take some time. You can optionally supply a bool
// which if true prevents any uploads. If a target was not configured with a
// specific CacheDir but CacheData was true, the CacheDir will be deleted.
func (fs *MuxFys) Unmount(doNotUpload ...bool) (err error) {
	fs.mutex.Lock()
	defer fs.mutex.Unlock()

	if fs.handlingSignals {
		fs.ignoreSignals <- true
	}

	if fs.mounted {
		err = fs.server.Unmount()
		if err == nil {
			fs.mounted = false
		}
	}

	if !(len(doNotUpload) > 0 && doNotUpload[0]) {
		// upload files that got opened for writing
		uerr := fs.uploadCreated()
		if uerr != nil {
			if err == nil {
				err = uerr
			} else {
				err = fmt.Errorf("%s; %s", err.Error(), uerr.Error())
			}
		}
	}

	// delete any cachedirs we created
	for _, remote := range fs.remotes {
		if remote.cacheIsTmp {
			remote.deleteCache()
		}
	}

	// clean out our caches; one reason to unmount is to force recognition of
	// new files when we re-mount
	fs.dirs = make(map[string][]*remote)
	fs.dirContents = make(map[string][]fuse.DirEntry)
	fs.files = make(map[string]*fuse.Attr)
	fs.fileToRemote = make(map[string]*remote)
	fs.createdFiles = make(map[string]bool)
	fs.createdDirs = make(map[string]bool)

	return
}

// uploadCreated uploads any files that previously got created. Only functions
// in CacheData mode.
func (fs *MuxFys) uploadCreated() error {
	if fs.writeRemote != nil && fs.writeRemote.cacheData {
		fails := 0

		// since mtimes in S3 are stored as the upload time, we sort our created
		// files by their mtime to at least upload them in the correct order
		var createdFiles []string
		for name := range fs.createdFiles {
			createdFiles = append(createdFiles, name)
		}
		if len(createdFiles) > 1 {
			sort.Slice(createdFiles, func(i, j int) bool {
				return fs.files[createdFiles[i]].Mtime < fs.files[createdFiles[j]].Mtime
			})
		}

		for _, name := range createdFiles {
			remotePath := fs.writeRemote.getRemotePath(name)
			localPath := fs.writeRemote.getLocalPath(remotePath)

			// upload file
			status := fs.writeRemote.uploadFile(localPath, remotePath)
			if status != fuse.OK {
				fails++
				continue
			}

			delete(fs.createdFiles, name)
		}

		if fails > 0 {
			return fmt.Errorf("failed to upload %d files\n", fails)
		}
	}
	return nil
}

// Logs returns messages generated while mounted; you might call it after
// Unmount() to see how things went. By default these will only be errors that
// occurred, but if this MuxFys was configured with Verbose on, it will also
// contain informational and warning messages. If the muxfys package was
// configured with a log Handler (see SetLogHandler()), these same messages
// would have been logged as they occurred.
func (fs *MuxFys) Logs() []string {
	return fs.logStore.Logs()
}

// SetLogHandler defines how log messages (globally for this package) are
// logged. Logs are always retrievable as strings from individual MuxFys
// instances using MuxFys.Logs(), but otherwise by default are discarded. To
// have them logged somewhere as they are emitted, supply a
// github.com/inconshreveable/log15 Handler, eg. log15.StderrHandler to log
// everything to STDERR.
func SetLogHandler(h log15.Handler) {
	logHandlerSetter.SetHandler(h)
}

// CacheTracker struct is used to track what parts of which files have been
// cached.
type CacheTracker struct {
	sync.Mutex
	cached map[string]Intervals
}

// NewCacheTracker creates a new *CacheTracker.
func NewCacheTracker() *CacheTracker {
	return &CacheTracker{cached: make(map[string]Intervals)}
}

// Cached updates the tracker with what you have now cached. Once you have
// stored bytes 0..9 in /abs/path/to/sparse.file, you would call:
// Cached("/abs/path/to/sparse.file", NewInterval(0, 10)).
func (c *CacheTracker) Cached(path string, iv Interval) {
	c.Lock()
	defer c.Unlock()
	c.cached[path] = c.cached[path].Merge(iv)
}

// Uncached tells you what parts of a file in the given interval you haven't
// already cached (based on your prior Cached() calls). You would want to then
// cache the data in each of the returned intervals and call Cached() on each
// one afterwards.
func (c *CacheTracker) Uncached(path string, iv Interval) Intervals {
	c.Lock()
	defer c.Unlock()
	return c.cached[path].Difference(iv)
}

// CacheTruncate should be used to update the tracker if you truncate a cache
// file. The internal knowledge of what you have cached for that file will then
// be updated to exclude anything beyond the truncation point.
func (c *CacheTracker) CacheTruncate(path string, offset int64) {
	c.Lock()
	defer c.Unlock()
	c.cached[path] = c.cached[path].Truncate(offset)
}

// CacheOverride should be used if you do something like delete a cache file and
// then recreate it and cache some data inside it. This is the slightly more
// efficient alternative to calling Delete(path) followed by Cached(path, iv).
func (c *CacheTracker) CacheOverride(path string, iv Interval) {
	c.Lock()
	defer c.Unlock()
	c.cached[path] = Intervals{iv}
}

// CacheRename should be used if you rename a cache file on disk.
func (c *CacheTracker) CacheRename(oldPath, newPath string) {
	c.Lock()
	defer c.Unlock()
	c.cached[newPath] = c.cached[oldPath]
	delete(c.cached, oldPath)
}

// CacheDelete should be used if you delete a cache file.
func (c *CacheTracker) CacheDelete(path string) {
	c.Lock()
	defer c.Unlock()
	delete(c.cached, path)
}

// CacheWipe should be used if you delete all your cache files.
func (c *CacheTracker) CacheWipe() {
	c.Lock()
	defer c.Unlock()
	c.cached = make(map[string]Intervals)
}
