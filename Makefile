BINARY := kami-gateway
SIDECAR := sidecar/claude-sdk-service.js
SIDECAR_PORT ?= 8081

.PHONY: build run run-gateway sidecar setup test fmt vet clean dist

build:
	go build -o $(BINARY) .

# `make run` starts the Claude SDK sidecar alongside the gateway, so the
# claude-sdk provider works out of the box, and stops it again on exit. The
# sidecar is optional — every other provider runs fine without it — so a
# missing `node` is a warning, not a failure.
run: build
	@if [ -z "$$(command -v node)" ]; then \
		echo "note: node not found — starting without the Claude SDK sidecar"; \
		echo "      (the claude-sdk provider will be unavailable)"; \
		./$(BINARY); \
	elif node -e 'require("net").connect($(SIDECAR_PORT),"127.0.0.1").on("connect",()=>process.exit(0)).on("error",()=>process.exit(1))' 2>/dev/null; then \
		echo "note: something already listens on 127.0.0.1:$(SIDECAR_PORT) — reusing it"; \
		./$(BINARY); \
	else \
		PORT=$(SIDECAR_PORT) node $(SIDECAR) & \
		sidecar_pid=$$!; \
		trap "kill $$sidecar_pid 2>/dev/null" EXIT INT TERM; \
		./$(BINARY); \
	fi

# The gateway on its own, for when the sidecar is already supervised elsewhere.
run-gateway: build
	./$(BINARY)

# The sidecar on its own, in the foreground.
sidecar:
	PORT=$(SIDECAR_PORT) node $(SIDECAR)

setup: build
	./$(BINARY) setup

test:
	go test ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

clean:
	rm -f $(BINARY)
	rm -rf dist

# macOS launchd service helpers
mac-install: build
	./setup-mac.sh

mac-start:
	launchctl load $(MAC_PLIST)

mac-stop:
	launchctl unload $(MAC_PLIST)

mac-restart: mac-stop mac-start

mac-logs:
	tail -f $(HOME)/kami-gateway/kami-gateway.log

# Cross-compile static binaries (no cgo, so they're self-contained).
dist: clean
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -o dist/$(BINARY)-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -o dist/$(BINARY)-linux-arm64 .
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o dist/$(BINARY)-darwin-arm64 .
	@echo "built:" && ls -1 dist
