BINARY    := breeze
INSTALL   := $(HOME)/.local/bin/$(BINARY)
BUILDTIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS   := -X main.buildTime=$(BUILDTIME)

.PHONY: all build install test vet fmt fmt-check race lint check clean run-daemon stop

all: check

## build the binary into ./breeze (gitignored)
build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

## build and install to ~/.local/bin, matching how it's actually deployed (see ci/deploy.sh)
install:
	go build -ldflags "$(LDFLAGS)" -o $(INSTALL) .

## full test suite, race detector on (what CI runs)
test:
	go test ./... -race -count=1

## same as test, explicit alias
race: test

vet:
	go vet ./...

fmt:
	gofmt -w .

## fails if anything isn't gofmt-clean, without rewriting it (CI-safe)
fmt-check:
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then \
		echo "gofmt needed on:"; echo "$$out"; exit 1; \
	fi

## everything build/deploy.sh and test.sh expect to pass before a commit is trusted
check: fmt-check vet test build

lint: check

clean:
	rm -f $(BINARY)

## run the daemon in the foreground (Ctrl-C to stop) — useful for watching logs live
run-daemon:
	go run . daemon

stop:
	-go run . stop
