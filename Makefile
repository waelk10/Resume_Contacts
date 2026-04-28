BINARY   := Resume_Contacts_Scraper
BIN_LINUX := $(BINARY).bin
BIN_WIN   := $(BINARY).exe

MODULE   := Resume_Contacts_Scraper
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS  := -s -w -X '$(MODULE)/cmd.version=$(VERSION)'

GO       := go
GOFLAGS  :=

.DEFAULT_GOAL := build

# ── Local build (current OS/arch) ───────────────────────────────────────────

.PHONY: build
build:
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) .

# ── Cross-compile targets ────────────────────────────────────────────────────

.PHONY: linux
linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_LINUX) .

.PHONY: windows
windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
		$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_WIN) .

.PHONY: all
all: linux windows

# ── Module hygiene ───────────────────────────────────────────────────────────

.PHONY: tidy
tidy:
	$(GO) mod tidy
	$(GO) mod verify

# ── Tests & linting ──────────────────────────────────────────────────────────

.PHONY: test
test:
	$(GO) test ./... -race -timeout 60s

.PHONY: lint
lint:
	golangci-lint run ./...

# ── Clean ────────────────────────────────────────────────────────────────────

.PHONY: clean
clean:
	$(GO) clean
	rm -f $(BINARY) $(BIN_LINUX) $(BIN_WIN)

# ── Help ─────────────────────────────────────────────────────────────────────

.PHONY: help
help:
	@echo "Targets:"
	@echo "  build    Build for the current OS/arch  →  $(BINARY)"
	@echo "  linux    Cross-compile for Linux amd64  →  $(BIN_LINUX)"
	@echo "  windows  Cross-compile for Windows amd64 → $(BIN_WIN)"
	@echo "  all      linux + windows"
	@echo "  tidy     go mod tidy && go mod verify"
	@echo "  test     Run tests with -race"
	@echo "  lint     Run golangci-lint"
	@echo "  clean    Remove built binaries"
