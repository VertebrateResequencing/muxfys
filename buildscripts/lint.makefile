# make -f buildscripts/lint.makefile

default: lint

test: export CGO_ENABLED = 0
test:
	@go test -p 1 -tags netgo --count 1 ./...

race: export CGO_ENABLED = 1
race:
	@go test -p 1 -tags netgo --count 1 -v -race ./...

# curl -sfL https://install.goreleaser.com/github.com/golangci/golangci-lint.sh | sh -s -- -b $(go env GOPATH)/bin v1.16.0
lint:
	@golangci-lint run

lintextra:
	@golangci-lint run -c .golangci_extra.yml

.PHONY: test race lint lintextra
