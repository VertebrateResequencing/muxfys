# Change Log
All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](http://keepachangelog.com/) and this
project adheres to [Semantic Versioning](http://semver.org/).


## [2.0.1] - 2017-07-19
### Changed
- V2 signatures are now used for compatibility with Ceph + latest version of
  minio-go.

### Fixed
- Non-existent remote directories can now be mounted and accessed without error.


## [2.0.0] - 2017-06-29
### Added
- Serial writes when not cached are now implemented.

### Changed
- RemoteAccessor public interface gains 3 new methods that must be implemented:
  UploadData(), DeleteIncompleteUpload() and ErrorIsNoQuota().

### Fixed
- Failed uploads no longer leave behind incomplete upload parts.
- Memory used for caches is freed on Flush() to avoid other calls running out of
  memory.


## [1.0.0] - 2017-05-19
### Added
- First semver release of muxfys
