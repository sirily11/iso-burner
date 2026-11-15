BINARY  ?= iso-burner
PREFIX  ?= /usr/local
BINDIR  ?= $(PREFIX)/bin
BUILD   := bin/$(BINARY)

# Installing into a root-owned directory needs sudo on Unix.
ifneq ($(OS),Windows_NT)
SUDO := $(shell test -w "$(BINDIR)" 2>/dev/null || echo sudo)
endif

.PHONY: all build install uninstall windows windows-install test clean

all: build

build:
	go build -trimpath -ldflags "-s -w" -o $(BUILD) .

install: build
	$(SUDO) install -d "$(BINDIR)"
	$(SUDO) install -m 0755 $(BUILD) "$(BINDIR)/$(BINARY)"
	@echo "Installed $(BINDIR)/$(BINARY)"

# From Windows PowerShell or a shell with GNU Make: make windows
windows-install:
	"$(SystemRoot)/System32/WindowsPowerShell/v1.0/powershell.exe" -NoProfile -ExecutionPolicy Bypass -File scripts/install-windows.ps1

windows: windows-install

uninstall:
	$(SUDO) rm -f "$(BINDIR)/$(BINARY)"

test:
	go test ./...

clean:
	rm -rf bin
