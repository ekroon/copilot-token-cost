BINARY   := copilot-token-cost
GO_DIR   := go
BUILD_DIR := dist

GOFLAGS  := -trimpath
LDFLAGS  := -s -w

# Cross-compilation matrix (matches release.yml)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: build release release-all embed-pricing clean test vet fmt desktop desktop-dev desktop-clean

# Dev build — current platform, no pricing embedded
build:
	cd $(GO_DIR) && go build -o $(BINARY) .

# Release build — current platform, pricing embedded
release: embed-pricing
	cd $(GO_DIR) && go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o ../$(BINARY) .

# Cross-compile all platforms
release-all: embed-pricing $(PLATFORMS)

$(PLATFORMS):
	$(eval GOOS   := $(word 1,$(subst /, ,$@)))
	$(eval GOARCH := $(word 2,$(subst /, ,$@)))
	@mkdir -p $(BUILD_DIR)
	cd $(GO_DIR) && GOOS=$(GOOS) GOARCH=$(GOARCH) \
		go build $(GOFLAGS) -ldflags="$(LDFLAGS)" \
		-o ../$(BUILD_DIR)/$(BINARY)-$(GOOS)-$(GOARCH) .
	@echo "Built $(BUILD_DIR)/$(BINARY)-$(GOOS)-$(GOARCH)"

embed-pricing:
	@printf 'package main\n\nvar embeddedPricingJSON = `' > $(GO_DIR)/pricing_data.go
	@cat pricing.json >> $(GO_DIR)/pricing_data.go
	@printf '`\n' >> $(GO_DIR)/pricing_data.go

clean:
	rm -rf $(BUILD_DIR)
	rm -f $(BINARY)
	rm -f $(GO_DIR)/$(BINARY)
	@# Restore dev pricing_data.go stub
	@printf 'package main\n\n// embeddedPricingJSON is populated at release build time via:\n//\n//\tgo generate ./...\n//\n// For local development, pricing.json is loaded from the filesystem.\n// The release workflow generates this file with the actual pricing data.\nvar embeddedPricingJSON = ""\n' > $(GO_DIR)/pricing_data.go

test:
	cd $(GO_DIR) && go test ./...

vet:
	cd $(GO_DIR) && go vet ./...

fmt:
	cd $(GO_DIR) && gofmt -w $$(find . -type f -name '*.go')

# Desktop app (Tauri) — macOS only
desktop:
	$(MAKE) -C desktop build

desktop-dev:
	$(MAKE) -C desktop dev

desktop-clean:
	$(MAKE) -C desktop clean
