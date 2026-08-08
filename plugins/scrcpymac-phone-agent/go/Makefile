# ScrcpyMac Phone Agent — Go build.
#
# Build order matters: scripts/build-ui.sh -> go build.
# The widget HTML is //go:embed-ed, so a Go build without a widget refresh
# silently ships the previous UI.

GO       ?= go
PLUGIN   := $(abspath $(CURDIR)/..)
WIDGET_DST := internal/widget/assets/scrcpymac-app.html
BIN_DIR  := $(PLUGIN)/bin/darwin
LDFLAGS  := -s -w

.PHONY: all build test vet fmt widget cross clean tidy check smoke contract

all: check build

## build: compile the binary for the host into ./phone-agent (not into the plugin's bin/).
build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o phone-agent ./cmd/phone-agent

## check: everything CI should run.
check: vet test

## contract: assert the registered MCP surface still matches docs/contract.json.
## Also runs as part of `test`; this target is for a fast, focused answer.
contract:
	$(GO) test ./internal/mcpserver/ -run TestContract -count=1 -v

## smoke: MCP handshake over stdio against a built binary.
## Pass BIN=path to smoke a release artefact instead of building from source.
smoke:
	scripts/smoke-stdio.sh $(BIN)

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy

## widget: build and refresh the embedded single-file widget.
widget:
	$(PLUGIN)/scripts/build-ui.sh

## cross: build both macOS binaries into the plugin's bin/ layout.
## scripts/build-go.sh does the same with version/commit stamping; prefer it for
## releases and use this for a quick local cross-check.
cross: widget
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" \
		-o $(BIN_DIR)/arm64/phone-agent ./cmd/phone-agent
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" \
		-o $(BIN_DIR)/x86_64/phone-agent ./cmd/phone-agent
	@ls -l $(BIN_DIR)/arm64/phone-agent $(BIN_DIR)/x86_64/phone-agent

clean:
	rm -f phone-agent
