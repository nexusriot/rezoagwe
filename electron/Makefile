# ---------------------------------------------------------------------------
# rezoagwe desktop client — build, test and packaging
#
# The app is a full peer, not a viewer: everything here produces something that
# joins a cluster on its own.
#
# Packaging comes in two flavours, and both are kept because they fail in
# different places:
#
#   make deb          electron-builder, which needs fpm (it downloads one into
#                     ~/.cache/electron-builder on first use) and can cross-build
#                     for arm64/armhf by fetching those Electron binaries
#   make deb-manual   dpkg-deb over an unpacked build, needing nothing but the
#                     dpkg tools already on any Debian box — the fallback for a
#                     machine with no network and no fpm
#
# Override the version:  make deb VERSION=0.2.0
# ---------------------------------------------------------------------------

NPM        ?= npm
NODE       ?= node
VERSION    ?= $(shell $(NODE) -p "require('./package.json').version" 2>/dev/null || echo 0.0.0)
DIST_DIR   := dist
BUILDER    := npx electron-builder
# Keeps the built artifacts and package.json in step when VERSION is overridden,
# rather than renaming a file that still reports the old version to --version.
VERSION_ARG := --config.extraMetadata.version=$(VERSION)

.DEFAULT_GOAL := help

.PHONY: help
help:
	@echo "rezoagwe desktop client (v$(VERSION)) targets:"
	@echo "  make install       - npm install (downloads Electron)"
	@echo "  make run | dev     - launch the app (dev opens devtools)"
	@echo "  make check         - syntax + shape check over every source file"
	@echo "  make test          - unit suite: a whole cluster in one process"
	@echo "  make interop       - build the Go binaries and replicate through them"
	@echo "  make selftest      - boot the real window and drive every screen"
	@echo "  make shots SHOTS_DIR=dir  - a PNG of every screen (needs no desktop)"
	@echo "  make icon          - regenerate the icon set into build/"
	@echo "  make binary        - unpacked runnable build -> $(DIST_DIR)/linux-unpacked/"
	@echo "  make tarball       - portable .tar.gz -> $(DIST_DIR)/"
	@echo "  make deb           - .deb for the host arch -> $(DIST_DIR)/"
	@echo "  make deb-arm64 | deb-armhf | debs   - cross-built .debs (downloads Electron)"
	@echo "  make deb-manual    - .deb via dpkg-deb only, no fpm and no network"
	@echo "  make appimage      - portable AppImage -> $(DIST_DIR)/"
	@echo "  make dist          - deb + AppImage for the host arch"
	@echo "  make verify        - check + test + selftest + a packaged-binary selftest"
	@echo "  make clean         - remove $(DIST_DIR)/, the generated icons and shots/"

node_modules: package.json
	$(NPM) install
	@touch node_modules

.PHONY: install
install:
	$(NPM) install

.PHONY: run
run: node_modules
	$(NPM) start

.PHONY: dev
dev: node_modules
	$(NPM) run dev

# ---- tests ---------------------------------------------------------------

.PHONY: check
check:
	$(NODE) scripts/check.js

# No sockets, no Electron, no display: the engine half depends on neither, so the
# multi-node tests run a cluster inside this process.
.PHONY: test
test: check
	$(NODE) --test test/*.test.js

# The one test that proves this client can join a *rezoagwe* cluster rather than
# a second copy of its own rules. Compiles the Go binaries itself.
.PHONY: interop
interop: node_modules
	REZOAGWE_GO_INTEROP=1 $(NODE) --test test/interop.test.js

.PHONY: selftest
selftest: node_modules
	$(NPM) run selftest

SHOTS_DIR ?= shots

.PHONY: shots
shots: node_modules icon
	npx electron . --screenshot $(SHOTS_DIR)

# ---- packaging -----------------------------------------------------------

.PHONY: icon
icon:
	$(NODE) scripts/make-icon.mjs

build/icon.png:
	$(NODE) scripts/make-icon.mjs

.PHONY: binary
binary: node_modules build/icon.png
	$(BUILDER) --dir $(VERSION_ARG)
	@echo ">> $(DIST_DIR)/linux-unpacked/rezoagwe-desktop"

.PHONY: tarball
tarball: node_modules build/icon.png
	$(BUILDER) --linux tar.gz $(VERSION_ARG)
	@ls $(DIST_DIR)/*.tar.gz 2>/dev/null | sed 's/^/>> /' || true

.PHONY: deb
deb: node_modules build/icon.png
	$(BUILDER) --linux deb $(VERSION_ARG)
	@ls $(DIST_DIR)/*.deb 2>/dev/null | sed 's/^/>> /' || true

.PHONY: deb-amd64
deb-amd64: node_modules build/icon.png
	$(BUILDER) --linux deb --x64 $(VERSION_ARG)

# Cross-building fetches the Electron binary for that architecture, so these
# need the network on a machine that has only ever built for the host.
.PHONY: deb-arm64
deb-arm64: node_modules build/icon.png
	$(BUILDER) --linux deb --arm64 $(VERSION_ARG)

.PHONY: deb-armhf
deb-armhf: node_modules build/icon.png
	$(BUILDER) --linux deb --armv7l $(VERSION_ARG)

.PHONY: debs
debs: deb-amd64 deb-arm64 deb-armhf

.PHONY: appimage
appimage: node_modules build/icon.png
	$(BUILDER) --linux AppImage $(VERSION_ARG)
	@ls $(DIST_DIR)/*.AppImage 2>/dev/null | sed 's/^/>> /' || true

.PHONY: dist
dist: node_modules build/icon.png
	$(BUILDER) --linux deb AppImage $(VERSION_ARG)

# The fallback path: an unpacked build laid out by hand and packed with dpkg-deb,
# for a machine that has the dpkg tools and nothing else.
.PHONY: deb-manual
deb-manual: binary
	VERSION=$(VERSION) ./build-deb.sh

# ---- verification --------------------------------------------------------

# Everything a change should have to survive: the suite, the real window, and
# the same window out of a packaged build — which is the only thing that proves
# the `files` list ships what the app actually loads.
.PHONY: verify
verify: test selftest binary
	$(DIST_DIR)/linux-unpacked/rezoagwe-desktop --selftest

.PHONY: clean
clean:
	rm -rf $(DIST_DIR) build/icon.png build/icons shots
