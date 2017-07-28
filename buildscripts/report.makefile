# make -f buildscripts/report.makefile

PKG := github.com/VertebrateResequencing/muxfys
PKG_LIST := $(shell go list ${PKG}/... | grep -v /vendor/)
GO_FILES := $(shell find . -name '*.go' | grep -v /vendor/)

default: report

test:
	@go test -p 1 -tags netgo -race ${PKG_LIST}

report: lint vet inef spell

lint:
	@for file in ${GO_FILES} ;  do \
		gofmt -s -l $$file ; \
		golint $$file ; \
	done

vet:
	@go vet ${PKG_LIST}

inef:
	@ineffassign ./

spell:
	@misspell ./

.PHONY: test report lint vet inef spell
