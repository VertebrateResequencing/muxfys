module github.com/VertebrateResequencing/muxfys/v4

require (
	github.com/alexflint/go-filemutex v1.0.0
	github.com/go-ini/ini v1.49.0
	github.com/go-stack/stack v1.8.0 // indirect
	github.com/gopherjs/gopherjs v0.0.0-20190915194858-d3ddacdb130f // indirect
	github.com/hanwen/go-fuse/v2 v2.0.2
	github.com/inconshreveable/log15 v0.0.0-20180818164646-67afb5ed74ec
	github.com/jpillora/backoff v1.0.0
	github.com/mattn/go-colorable v0.1.4 // indirect
	github.com/mattn/go-isatty v0.0.10 // indirect
	github.com/minio/minio-go/v7 v7.0.10
	github.com/mitchellh/go-homedir v1.1.0
	github.com/sb10/l15h v0.0.0-20170510122137-64c488bf8e22
	github.com/smartystreets/assertions v1.0.1 // indirect
	github.com/smartystreets/goconvey v1.6.4
	gopkg.in/check.v1 v1.0.0-20190902080502-41f04d3bba15 // indirect
)

replace github.com/hanwen/go-fuse/v2 => github.com/sb10/go-fuse/v2 v2.0.3-0.20191025142439-7d7db5160cb6

go 1.13
