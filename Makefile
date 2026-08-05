GO ?= go
BINARY ?= tsnet-proxy
APP_VERSION ?= 0.1.0
TAILSCALE_VERSION ?= 1.102.2
TAILSCALE_LONG_STAMP ?= $(TAILSCALE_VERSION)-tsnet-proxy-$(APP_VERSION)
TAILSCALE_SHORT_STAMP ?= $(TAILSCALE_VERSION)
LDFLAGS := -X main.version=$(APP_VERSION) -X tailscale.com/version.longStamp=$(TAILSCALE_LONG_STAMP) -X tailscale.com/version.shortStamp=$(TAILSCALE_SHORT_STAMP)
STRINGS ?= strings

.PHONY: build test vet fmt check cross-build verify-stamp

build:
	mkdir -p $(dir $(BINARY))
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/tsnet-proxy

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

check: test vet
	@test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*'))"

cross-build:
	mkdir -p .tmp-build
	GOOS=linux GOARCH=amd64 $(GO) build -ldflags "$(LDFLAGS)" -o .tmp-build/tsnet-proxy-linux-amd64 ./cmd/tsnet-proxy
	GOOS=darwin GOARCH=arm64 $(GO) build -ldflags "$(LDFLAGS)" -o .tmp-build/tsnet-proxy-darwin-arm64 ./cmd/tsnet-proxy
	GOOS=windows GOARCH=amd64 $(GO) build -ldflags "$(LDFLAGS)" -o .tmp-build/tsnet-proxy-windows-amd64.exe ./cmd/tsnet-proxy

verify-stamp: build
	$(STRINGS) "$(BINARY)" | grep -F "$(TAILSCALE_LONG_STAMP)"
