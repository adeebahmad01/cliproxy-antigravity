BINARY := cliproxy-antigravity
VERSION ?= 0.1.3
DIST ?= dist
UNAME_S := $(shell uname -s)
UNAME_M := $(shell uname -m)

ifeq ($(GOOS),)
  ifeq ($(UNAME_S),Darwin)
    GOOS := darwin
    EXT := dylib
  else
    GOOS := linux
    EXT := so
  endif
else
  ifeq ($(GOOS),windows)
    EXT := dll
  else ifeq ($(GOOS),darwin)
    EXT := dylib
  else
    EXT := so
  endif
endif

ifeq ($(GOARCH),)
  ifeq ($(UNAME_M),x86_64)
    GOARCH := amd64
  else ifeq ($(UNAME_M),aarch64)
    GOARCH := arm64
  else ifeq ($(UNAME_M),arm64)
    GOARCH := arm64
  else
    GOARCH := $(shell go env GOARCH)
  endif
endif

.PHONY: fmt test build smoke clean package

fmt:
	gofmt -w *.go scripts/*.go

test:
	CGO_ENABLED=1 go test -v ./...

build:
	CGO_ENABLED=1 go build -buildmode=c-shared -o $(BINARY).$(EXT) .

smoke: build
	python3 scripts/abi_smoke.py ./$(BINARY).$(EXT)

package:
	mkdir -p $(DIST)
	CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -buildmode=c-shared -ldflags="-s -w" -o $(DIST)/$(BINARY).$(EXT) .
	rm -f $(DIST)/$(BINARY).h
	go run scripts/package-release.go -library $(DIST)/$(BINARY).$(EXT) -archive $(DIST)/$(BINARY)_$(VERSION)_$(GOOS)_$(GOARCH).zip -checksum $(DIST)/$(BINARY)_$(VERSION)_$(GOOS)_$(GOARCH).zip.sha256

clean:
	rm -rf $(BINARY).so $(BINARY).dylib $(BINARY).dll $(BINARY).h $(DIST)
