# make -f buildscripts/lint.makefile

default: lint

test: export CGO_ENABLED = 0
test:
	@go test -p 1 -tags netgo --count 1 ./...

race: export CGO_ENABLED = 1
race:
	@go test -p 1 -tags netgo --count 1 -v -race ./...

# cd $(go env GOPATH); curl -L https://git.io/vp6lP | sh
# until all go tools have module support:
# mkdir -p $HOME/go/src
# go mod vendor
# rsync -a vendor/ ~/go/src/
# rm -fr vendor
# ln -s $PWD $HOME/go/src/github.com/VertebrateResequencing/muxfys
lint: export GO111MODULE = off
lint:
	@gometalinter --vendor --aggregate --deadline=120s ./... | sort

lint: export GO111MODULE = off
lintextra:
	@gometalinter --vendor --aggregate --deadline=120s --disable-all --enable=gocyclo --enable=dupl ./... | sort

.PHONY: test race lint lintextra
