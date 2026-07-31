SHELL := /bin/sh

BINARY := keepawake
DIST_DIR := dist
PREFIX ?= $(HOME)/.local
BIN_DIR ?= $(PREFIX)/bin

.PHONY: all build install test test-race vet fmt-check verify clean

all: verify

build:
	mkdir -p "$(DIST_DIR)"
	go build -trimpath -o "$(DIST_DIR)/$(BINARY)" .

install: build
	mkdir -p "$(BIN_DIR)"
	install -m 0755 "$(DIST_DIR)/$(BINARY)" "$(BIN_DIR)/$(BINARY)"

test:
	go test ./...

test-race:
	go test -race -cover ./...

vet:
	go vet ./...

fmt-check:
	test -z "$$(gofmt -l .)"

verify: fmt-check vet test-race

clean:
	rm -rf "$(DIST_DIR)"
