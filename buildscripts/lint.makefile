# make -f buildscripts/lint.makefile

PKG := github.com/VertebrateResequencing/muxfys
PKG_LIST := $(shell go list ${PKG}/... | grep -v /vendor/)
GO_FILES := $(shell find . -name '*.go' | grep -v /vendor/)

default: lint

test: export CGO_ENABLED = 0
test:
	@go test -p 1 -tags netgo ${PKG_LIST}

race: export CGO_ENABLED = 1
race:
	@go test -p 1 -tags netgo -race ${PKG_LIST}

# go get -u gopkg.in/alecthomas/gometalinter.v2
# gometalinter.v2 --install
lint:
	@gometalinter.v2 --vendor --aggregate --deadline=120s ./... | sort

lintextra:
	@gometalinter.v2 --vendor --aggregate --deadline=120s --disable-all --enable=gocyclo --enable=dupl ./... | sort

.PHONY: test race lint lintextra
