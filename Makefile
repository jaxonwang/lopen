GO ?= go

.PHONY: all build test lint clean remote darwin linux windows

all: build test

build: remote darwin linux windows

# Remote-side client (pushed to hosts by `lopend setup`); remotes are POSIX.
remote:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -o dist/lopen-linux-amd64 ./cmd/lopen
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build -trimpath -o dist/lopen-linux-arm64 ./cmd/lopen

# Local-side daemon, per platform.
darwin:
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 $(GO) build -trimpath -o dist/lopend-darwin-arm64 ./cmd/lopend
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -o dist/lopend-darwin-amd64 ./cmd/lopend

linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -o dist/lopend-linux-amd64 ./cmd/lopend
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build -trimpath -o dist/lopend-linux-arm64 ./cmd/lopend

windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -o dist/lopend-windows-amd64.exe ./cmd/lopend
	GOOS=windows GOARCH=arm64 CGO_ENABLED=0 $(GO) build -trimpath -o dist/lopend-windows-arm64.exe ./cmd/lopend

test:
	$(GO) test ./...

lint:
	$(GO) vet ./...
	gofmt -l . | (! grep .)

clean:
	rm -rf dist
