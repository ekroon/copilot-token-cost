ARCH := $(shell uname -m)
ifeq ($(ARCH),arm64)
  TARGET_TRIPLE := aarch64-apple-darwin
else ifeq ($(ARCH),x86_64)
  TARGET_TRIPLE := x86_64-apple-darwin
else
  $(error Unsupported architecture: $(ARCH))
endif

GO_SRC := ../go
SIDECAR_BIN := src-tauri/binaries/copilot-token-cost-$(TARGET_TRIPLE)
PRICING_SRC := ../pricing.json
PRICING_DATA := $(GO_SRC)/pricing_data.go
PRICING_DATA_BAK := $(GO_SRC)/pricing_data.go.bak

.PHONY: all build dev build-sidecar install-deps clean

all: build

install-deps:
	npm install

build-sidecar:
	@echo "==> Embedding pricing data into Go binary..."
	@cp $(PRICING_DATA) $(PRICING_DATA_BAK)
	@printf 'package main\n\nvar embeddedPricingJSON = `' > $(PRICING_DATA)
	@cat $(PRICING_SRC) >> $(PRICING_DATA)
	@printf '`\n' >> $(PRICING_DATA)
	@echo "==> Building Go sidecar for $(TARGET_TRIPLE)..."
	cd $(GO_SRC) && GOOS=darwin GOARCH=$(if $(filter aarch64-apple-darwin,$(TARGET_TRIPLE)),arm64,amd64) \
		go build -trimpath -ldflags="-s -w" -o ../desktop/$(SIDECAR_BIN) .
	@echo "==> Restoring pricing_data.go..."
	@mv $(PRICING_DATA_BAK) $(PRICING_DATA)
	@echo "==> Sidecar built: $(SIDECAR_BIN)"

dev: build-sidecar
	npx tauri dev

build: build-sidecar
	npx tauri build

clean:
	rm -f $(SIDECAR_BIN)
	rm -f $(PRICING_DATA_BAK)
	rm -rf src-tauri/target
