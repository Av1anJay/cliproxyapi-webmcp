.PHONY: all clean linux darwin windows test fmt vet check

PLUGIN_NAME := webmcp
GO ?= go

all: linux

linux: bin/$(PLUGIN_NAME).so

darwin: bin/$(PLUGIN_NAME).dylib

windows: bin/$(PLUGIN_NAME).dll

bin/$(PLUGIN_NAME).so: $(wildcard *.go) go.mod
	@mkdir -p bin
	CGO_ENABLED=1 $(GO) build -trimpath -buildmode=c-shared -o bin/$(PLUGIN_NAME).so .
	@rm -f bin/$(PLUGIN_NAME).h

bin/$(PLUGIN_NAME).dylib: $(wildcard *.go) go.mod
	@mkdir -p bin
	CGO_ENABLED=1 $(GO) build -trimpath -buildmode=c-shared -o bin/$(PLUGIN_NAME).dylib .
	@rm -f bin/$(PLUGIN_NAME).h

bin/$(PLUGIN_NAME).dll: $(wildcard *.go) go.mod
	@mkdir -p bin
	CGO_ENABLED=1 $(GO) build -trimpath -buildmode=c-shared -o bin/$(PLUGIN_NAME).dll .
	@rm -f bin/$(PLUGIN_NAME).h

test:
	CGO_ENABLED=1 $(GO) test -count=1 ./...

check: fmt vet
	CGO_ENABLED=1 $(GO) test -race -count=1 ./...

fmt:
	gofmt -w .

vet:
	CGO_ENABLED=1 $(GO) vet ./...

clean:
	rm -rf bin dist
