# ScrcpyMac Phone Agent — Go build.
#
# Build order matters: scripts/build-ui.sh -> make widget -> go build.
# The widget HTML is //go:embed-ed, so a Go build without a widget refresh
# silently ships the previous UI.

GO       ?= go
PLUGIN   := $(abspath $(CURDIR)/..)
WIDGET_SRC := $(PLUGIN)/server/phone_agent/static/scrcpymac-app.html
WIDGET_DST := internal/widget/assets/scrcpymac-app.html
BIN_DIR  := $(PLUGIN)/bin/darwin
LDFLAGS  := -s -w

.PHONY: all build test vet fmt widget widget-check cross clean tidy check smoke contract parity parity-selftest

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

## parity: run BOTH implementations against the attached device and diff the
## results. Needs a device and the plugin's Python .venv; not part of `check`.
## Pass ARGS='--cases doctor,devices' to narrow it.
parity:
	scripts/parity/run-parity.sh $(ARGS)

## parity-selftest: prove the parity harness rejects a broken port. No device,
## no Go binary — perturbs real captured payloads and asserts each is caught.
parity-selftest:
	python3 scripts/parity/selftest.py

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy

## widget: refresh the embedded widget from the UI build output.
## Run scripts/build-ui.sh first — this only copies what that produced.
widget:
	@if [ ! -f "$(WIDGET_SRC)" ]; then \
		echo "ERROR: $(WIDGET_SRC) is missing. Run scripts/build-ui.sh first." >&2; \
		exit 1; \
	fi
	@mkdir -p $(dir $(WIDGET_DST))
	@if cmp -s "$(WIDGET_SRC)" "$(WIDGET_DST)"; then \
		echo "widget already up to date ($(WIDGET_DST))"; \
	else \
		cp "$(WIDGET_SRC)" "$(WIDGET_DST)"; \
		echo "refreshed $(WIDGET_DST)"; \
	fi

## widget-check: fail if the embedded widget is stale. Wire this into CI so a UI
## change cannot ship without a rebuilt binary.
widget-check:
	@if [ ! -f "$(WIDGET_SRC)" ]; then \
		echo "SKIP: no UI build output to compare against" >&2; \
	elif cmp -s "$(WIDGET_SRC)" "$(WIDGET_DST)"; then \
		echo "embedded widget is current"; \
	else \
		echo "ERROR: $(WIDGET_DST) is stale. Run: make widget" >&2; \
		exit 1; \
	fi

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
