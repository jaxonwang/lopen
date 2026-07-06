GO ?= go

.PHONY: all build test lint clean darwin linux

all: build test

build: linux darwin

linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -o dist/lopen-linux-amd64 ./cmd/lopen
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build -trimpath -o dist/lopen-linux-arm64 ./cmd/lopen

darwin:
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 $(GO) build -trimpath -o dist/lopend-darwin-arm64 ./cmd/lopend
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -o dist/lopend-darwin-amd64 ./cmd/lopend

test:
	$(GO) test ./...

lint:
	$(GO) vet ./...
	gofmt -l . | (! grep .)

clean:
	rm -rf dist
