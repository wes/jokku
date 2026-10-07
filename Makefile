VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X github.com/wes/jokku/internal/version.Version=$(VERSION) -X github.com/wes/jokku/internal/version.Commit=$(COMMIT)
DEV     := $(CURDIR)/.dev

.PHONY: build linux test dev

build:
	go build -ldflags "$(LDFLAGS)" -o bin/jokku ./cmd/jokku

# Static binaries for servers.
linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/jokku-linux-amd64 ./cmd/jokku
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/jokku-linux-arm64 ./cmd/jokku

test:
	go test ./...

# Run a control plane on this machine with state in .dev/. In another shell:
#   export JOKKU_SOCKET=$PWD/.dev/jokku.sock
#   bin/jokku apps:create myapp
dev: build
	mkdir -p $(DEV)
	bin/jokku daemon --data-dir $(DEV)/data --socket $(DEV)/jokku.sock --git-user "" --authorized-keys $(DEV)/authorized_keys
