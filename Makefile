BINARY  ?= iso-bunner
PREFIX  ?= /usr/local
BINDIR  ?= $(PREFIX)/bin
BUILD   := bin/$(BINARY)

# Installing into a root-owned directory needs sudo; skip it when writable.
SUDO := $(shell test -w "$(BINDIR)" 2>/dev/null || echo sudo)

.PHONY: all build install uninstall test clean

all: build

build:
	go build -trimpath -ldflags "-s -w" -o $(BUILD) .

install: build
	$(SUDO) install -d "$(BINDIR)"
	$(SUDO) install -m 0755 $(BUILD) "$(BINDIR)/$(BINARY)"
	@echo "Installed $(BINDIR)/$(BINARY)"

uninstall:
	$(SUDO) rm -f "$(BINDIR)/$(BINARY)"

test:
	go test ./...

clean:
	rm -rf bin
