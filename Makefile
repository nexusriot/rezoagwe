# ---------------------------------------------------------------------------
# rezoagwe Makefile
#
# Cross-build targets + Debian packaging.
#
# The project produces TWO binaries:
#   * rezoagwe-bootstrap  — bootstrap node (cmd/bootstrap)
#   * rezoagwe-discovery  — discovery / TUI node (cmd/discovery)
#
# Supported build types (both binaries are built per type):
#
#   x86_64            linux/amd64            (default toolchain, may link libc)
#   x86_64-static     linux/amd64            (CGO disabled -> fully static)
#   linux-i686        linux/386              (32-bit x86, static)
#   freebsd-x86_64    freebsd/amd64          (static)
#   uconsole          linux/arm64            (ClockworkPi uConsole, CM4)
#   pizero2w          linux/arm64            (Raspberry Pi Zero 2 W, 64-bit OS)
#   pizero2w-armhf    linux/arm  GOARM=7     (Pi Zero 2 W, 32-bit Raspberry Pi OS)
#   licheerv          linux/riscv64          (LicheeRV Nano (W), riscv64)
#   darwin            darwin/amd64 + arm64
#   windows           windows/amd64
#
# Debian packages (.deb via dpkg-deb) — each .deb ships BOTH binaries:
#   deb-amd64  deb-i386  deb-arm64  deb-armhf  deb-riscv64  ->  build/<pkg>.deb
#
# Override version:   make debs VERSION=0.0.3
# ---------------------------------------------------------------------------

APP        := rezoagwe
BIN_BOOT   := rezoagwe-bootstrap
BIN_DISC   := rezoagwe-discovery
PKG_BOOT   := ./cmd/bootstrap
PKG_DISC   := ./cmd/discovery
GO         ?= go
VERSION    ?= 0.0.3
LDFLAGS    ?= -s -w
BUILD_DIR  := build
DIST_DIR   := dist

# go_build_pair: $(1)=GOOS $(2)=GOARCH $(3)=suffix $(4)=CGO_ENABLED $(5)=GOARM(optional) $(6)=exe-ext
define go_build_pair
	@mkdir -p $(DIST_DIR)
	CGO_ENABLED=$(4) GOOS=$(1) GOARCH=$(2) $(if $(5),GOARM=$(5),) \
		$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BIN_BOOT)-$(VERSION)-$(3)$(6) $(PKG_BOOT)
	@echo ">> $(DIST_DIR)/$(BIN_BOOT)-$(VERSION)-$(3)$(6)"
	CGO_ENABLED=$(4) GOOS=$(1) GOARCH=$(2) $(if $(5),GOARM=$(5),) \
		$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BIN_DISC)-$(VERSION)-$(3)$(6) $(PKG_DISC)
	@echo ">> $(DIST_DIR)/$(BIN_DISC)-$(VERSION)-$(3)$(6)"
endef

# build_deb: $(1)=deb-arch $(2)=GOARCH $(3)=GOARM(optional)
define build_deb
	@command -v dpkg-deb >/dev/null 2>&1 || { echo "ERROR: dpkg-deb not found (install the 'dpkg' package)"; exit 1; }
	@mkdir -p $(BUILD_DIR)
	@rm -rf "$(BUILD_DIR)/$(APP)_$(VERSION)_$(1)"
	@mkdir -p "$(BUILD_DIR)/$(APP)_$(VERSION)_$(1)/usr/bin"
	@cp -r DEBIAN "$(BUILD_DIR)/$(APP)_$(VERSION)_$(1)/DEBIAN"
	CGO_ENABLED=0 GOOS=linux GOARCH=$(2) $(if $(3),GOARM=$(3),) \
		$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "$(BUILD_DIR)/$(APP)_$(VERSION)_$(1)/usr/bin/$(BIN_BOOT)" $(PKG_BOOT)
	CGO_ENABLED=0 GOOS=linux GOARCH=$(2) $(if $(3),GOARM=$(3),) \
		$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "$(BUILD_DIR)/$(APP)_$(VERSION)_$(1)/usr/bin/$(BIN_DISC)" $(PKG_DISC)
	@chmod 0755 "$(BUILD_DIR)/$(APP)_$(VERSION)_$(1)/usr/bin/$(BIN_BOOT)" "$(BUILD_DIR)/$(APP)_$(VERSION)_$(1)/usr/bin/$(BIN_DISC)"
	@sed -i "s/_version_/$(VERSION)/g" "$(BUILD_DIR)/$(APP)_$(VERSION)_$(1)/DEBIAN/control"
	@sed -i "s/^Architecture: .*/Architecture: $(1)/" "$(BUILD_DIR)/$(APP)_$(VERSION)_$(1)/DEBIAN/control"
	cd $(BUILD_DIR) && dpkg-deb --build -Z gzip --root-owner-group "$(APP)_$(VERSION)_$(1)"
	@echo ">> $(BUILD_DIR)/$(APP)_$(VERSION)_$(1).deb"
endef

.DEFAULT_GOAL := help

.PHONY: help
help:
	@echo "rezoagwe build targets (VERSION=$(VERSION)):"
	@echo "  make all                - all binary build types into $(DIST_DIR)/"
	@echo "  make x86_64             - linux/amd64 (default toolchain)"
	@echo "  make x86_64-static      - linux/amd64 fully static (CGO off)"
	@echo "  make linux-i686         - linux/386 (32-bit x86, static)"
	@echo "  make freebsd-x86_64     - freebsd/amd64 static"
	@echo "  make uconsole           - linux/arm64 (ClockworkPi uConsole CM4)"
	@echo "  make pizero2w           - linux/arm64 (Pi Zero 2 W, 64-bit OS)"
	@echo "  make pizero2w-armhf     - linux/arm v7 (Pi Zero 2 W, 32-bit OS)"
	@echo "  make licheerv           - linux/riscv64 (LicheeRV Nano (W))"
	@echo "  make darwin windows     - macOS / Windows"
	@echo "  make debs               - deb-amd64 + deb-i386 + deb-arm64 + deb-armhf + deb-riscv64"
	@echo "  make test | test-race | vet | fmt | tidy | clean"

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: fmt
fmt:
	$(GO) fmt ./...

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: test
test:
	$(GO) test ./...

.PHONY: test-race
test-race:
	$(GO) test -race -count=1 ./...

.PHONY: clean
clean:
	rm -rf $(BUILD_DIR) $(DIST_DIR) $(BIN_BOOT) $(BIN_DISC)

.PHONY: all
all: x86_64 x86_64-static linux-i686 freebsd-x86_64 uconsole pizero2w pizero2w-armhf licheerv darwin windows

.PHONY: x86_64
x86_64:
	$(call go_build_pair,linux,amd64,linux-amd64,1)

.PHONY: x86_64_static
x86_64_static: x86_64-static

.PHONY: x86_64-static
x86_64-static:
	$(call go_build_pair,linux,amd64,linux-amd64-static,0)

.PHONY: linux-i686 linux_i686
linux_i686: linux-i686
linux-i686:
	$(call go_build_pair,linux,386,linux-i686,0)

.PHONY: freebsd-x86_64
freebsd-x86_64:
	$(call go_build_pair,freebsd,amd64,freebsd-amd64,0)

.PHONY: uconsole
uconsole:
	$(call go_build_pair,linux,arm64,uconsole-linux-arm64,0)

.PHONY: pizero2w
pizero2w:
	$(call go_build_pair,linux,arm64,pizero2w-linux-arm64,0)

.PHONY: pizero2w-armhf
pizero2w-armhf:
	$(call go_build_pair,linux,arm,pizero2w-linux-armv7,0,7)

.PHONY: licheerv
licheerv:
	$(call go_build_pair,linux,riscv64,licheerv-linux-riscv64,0)

.PHONY: darwin
darwin:
	$(call go_build_pair,darwin,amd64,darwin-amd64,0)
	$(call go_build_pair,darwin,arm64,darwin-arm64,0)

.PHONY: windows
windows:
	$(call go_build_pair,windows,amd64,windows-amd64,0,,.exe)

.PHONY: debs
debs: deb-amd64 deb-i386 deb-arm64 deb-armhf deb-riscv64

.PHONY: deb-amd64
deb-amd64:
	$(call build_deb,amd64,amd64)

.PHONY: deb-i386
deb-i386:
	$(call build_deb,i386,386)

# arm64 deb works for both ClockworkPi uConsole (CM4) and Pi Zero 2 W (64-bit).
.PHONY: deb-arm64
deb-arm64:
	$(call build_deb,arm64,arm64)

# armhf deb for 32-bit Raspberry Pi OS on the Pi Zero 2 W.
.PHONY: deb-armhf
deb-armhf:
	$(call build_deb,armhf,arm,7)

# riscv64 deb for the LicheeRV Nano (W).
.PHONY: deb-riscv64
deb-riscv64:
	$(call build_deb,riscv64,riscv64)
