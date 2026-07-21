BINARY   := kami-gateway
MAC_PLIST := $(HOME)/Library/LaunchAgents/com.kami.gateway.plist

.PHONY: build run setup test fmt vet clean dist \
        mac-install mac-start mac-stop mac-restart mac-logs

build:
	go build -o $(BINARY) .

run: build
	./$(BINARY)

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
