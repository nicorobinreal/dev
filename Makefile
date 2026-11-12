.PHONY: build test lint snapshot install e2e clean

VERSION ?= dev
BIN     := bin/dev
LDFLAGS := -s -w -X main.version=$(VERSION)
PREFIX  ?= $(HOME)/.local/bin

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/dev

test:
	DEV_NO_UPDATE=1 go test -race ./...

lint:
	golangci-lint run ./...

snapshot:
	goreleaser build --snapshot --clean

# Atomic install: write next to the target, then rename over it.
install: build
	mkdir -p $(PREFIX)
	cp $(BIN) $(PREFIX)/.dev.tmp
	chmod 755 $(PREFIX)/.dev.tmp
	mv -f $(PREFIX)/.dev.tmp $(PREFIX)/dev

# Real VMs on this Mac. Dedicated machine prefix "e2e" so test VMs and tailnet
# nodes are easy to spot; no auto-update during the run.
e2e:
	DEV_NO_UPDATE=1 DEV_MACHINE=e2e go test -tags e2e ./e2e/... -v -count=1 -timeout 60m

clean:
	rm -rf bin dist
