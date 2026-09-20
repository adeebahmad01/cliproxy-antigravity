BINARY := cliproxy-antigravity
UNAME_S := $(shell uname -s)

ifeq ($(UNAME_S),Darwin)
EXT := dylib
else
EXT := so
endif

.PHONY: fmt test build smoke clean

fmt:
	gofmt -w *.go

test:
	go test ./...

build:
	CGO_ENABLED=1 go build -buildmode=c-shared -o $(BINARY).$(EXT) .

smoke: build
	python3 scripts/abi_smoke.py ./$(BINARY).$(EXT)

clean:
	rm -f $(BINARY).so $(BINARY).dylib $(BINARY).dll $(BINARY).h
