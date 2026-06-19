GOLANGCI_LINT_VERSION := v2.12.2
GO_FILES := $(shell find . -name '*.go' -not -path './vendor/*')
MUXFYS_S3_PORT ?= $(shell python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()' 2>/dev/null || echo 9000)

export MUXFYS_S3_PORT

default: test

lint:
	@test -z "$$(gofmt -l $(GO_FILES))"
	@go mod tidy -diff
	@go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run --timeout=5m

lint-fix:
	@go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run --fix --timeout=5m

test: export CGO_ENABLED = 0
test:
	@go test -p 1 -tags netgo -timeout 30m --count 1 ./...

race: export CGO_ENABLED = 1
race:
	@go test -p 1 -tags netgo -race -timeout 30m --count 1 ./...

coverage: export CGO_ENABLED = 1
coverage:
	@go test -tags netgo -v -coverprofile=muxfys.coverprofile -covermode count .

gates: lint test race coverage

.PHONY: default lint lint-fix test race coverage gates
