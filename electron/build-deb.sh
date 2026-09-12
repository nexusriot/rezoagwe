#!/usr/bin/env bash
# Packs an already-built Electron app into a .deb with nothing but dpkg-deb.
#
# electron-builder can do this itself (`make deb`), but it wants fpm, which it
# downloads on first use — so on a machine with no network and no fpm the deb
# is simply unbuildable. This path needs only the dpkg tools that any Debian box
# already has, and it lays the package out the way the distributions expect:
# the app under /opt, a launcher on the PATH, a desktop entry and an icon.
#
#   ./build-deb.sh                 # host arch, version from package.json
#   VERSION=0.2.0 ./build-deb.sh   # override the version
#   ARCH=arm64 ./build-deb.sh dist/linux-arm64-unpacked
set -euo pipefail

cd "$(dirname "$0")"

command -v dpkg-deb >/dev/null 2>&1 || {
    echo "dpkg-deb not found (install the 'dpkg' package)" >&2
    exit 1
}

VERSION="${VERSION:-$(node -p "require('./package.json').version")}"
PKG_NAME="rezoagwe-desktop"
EXE="rezoagwe-desktop"
# The same prefix electron-builder uses (it installs under the product name), so
# a deb from either path is the same package and upgrading between them is clean.
INSTALL_DIR="/opt/Rezoagwe"

# The unpacked build to pack. electron-builder names the directory after the
# architecture, and the host build is the unsuffixed one.
UNPACKED="${1:-}"
if [ -z "$UNPACKED" ]; then
    for candidate in dist/linux-unpacked dist/linux-x64-unpacked dist/linux-arm64-unpacked; do
        [ -d "$candidate" ] && UNPACKED="$candidate" && break
    done
fi
if [ -z "$UNPACKED" ] || [ ! -d "$UNPACKED" ]; then
    echo "no unpacked build found — run 'make binary' first (or pass the directory)" >&2
    exit 1
fi
[ -x "$UNPACKED/$EXE" ] || {
    echo "$UNPACKED/$EXE is missing: that directory is not an unpacked build of this app" >&2
    exit 1
}

# Derive the Debian architecture from the build unless told otherwise, so a deb
# never claims to be for the machine that happened to pack it.
if [ -z "${ARCH:-}" ]; then
    case "$UNPACKED" in
        *arm64*) ARCH=arm64 ;;
        *armv7l*|*armhf*) ARCH=armhf ;;
        *ia32*) ARCH=i386 ;;
        *) ARCH="$(dpkg --print-architecture 2>/dev/null || echo amd64)" ;;
    esac
fi

STAGE="dist/${PKG_NAME}_${VERSION}_${ARCH}"
echo "packing $UNPACKED -> ${STAGE}.deb"
rm -rf "$STAGE"
mkdir -p "$STAGE/DEBIAN" "$STAGE$INSTALL_DIR" "$STAGE/usr/bin" \
    "$STAGE/usr/share/applications" "$STAGE/usr/share/doc/$PKG_NAME"

cp -a "$UNPACKED/." "$STAGE$INSTALL_DIR/"
# chrome-sandbox has to be setuid root or Electron refuses to start under a
# kernel without unprivileged user namespaces; dpkg-deb cannot set that bit from
# a non-root build, so the postinst does it.
chmod 0755 "$STAGE$INSTALL_DIR/$EXE"

ln -sf "$INSTALL_DIR/$EXE" "$STAGE/usr/bin/$EXE"
# Every size a desktop looks in, not just the master: an icon installed under
# one size is an icon most menus never find.
for icon in build/icons/*.png; do
    size="$(basename "$icon" .png)"
    mkdir -p "$STAGE/usr/share/icons/hicolor/$size/apps"
    cp "$icon" "$STAGE/usr/share/icons/hicolor/$size/apps/${PKG_NAME}.png"
done

cat > "$STAGE/usr/share/applications/${PKG_NAME}.desktop" <<EOF
[Desktop Entry]
Type=Application
Name=Rezoagwe
GenericName=Distributed KV store node
Comment=Join a rezoagwe cluster: keys, chat, peers and the cluster graph
Exec=$EXE %U
Icon=$PKG_NAME
Terminal=false
Categories=Network;P2P;Utility;
Keywords=rezoagwe;cluster;kv;gossip;p2p;chat;
StartupWMClass=Rezoagwe
EOF

INSTALLED_KB="$(du -sk "$STAGE" | cut -f1)"

cat > "$STAGE/DEBIAN/control" <<EOF
Package: $PKG_NAME
Version: $VERSION
Section: net
Priority: optional
Architecture: $ARCH
Maintainer: Vladislav Ananev <nexus.riot@gmail.com>
Installed-Size: $INSTALLED_KB
Depends: libgtk-3-0, libnotify4, libnss3, libxss1, libxtst6, xdg-utils, libatspi2.0-0, libuuid1, libsecret-1-0
Description: Desktop client for the rezoagwe distributed KV store
 A full peer node and the rendezvous service in one application: the store,
 the replication, the chat and the anti-entropy all run in the app, so it is
 a member of the cluster rather than a window onto someone else's node.
 .
 The cluster is drawn as a graph derived from the peer lists gossip already
 carries, so a partition or a link only one end claims is visible rather than
 implied, and a diagnostics screen turns the protocol counters into causes.
EOF

cat > "$STAGE/DEBIAN/postinst" <<'EOF'
#!/bin/sh
set -e
# Electron's sandbox helper needs the setuid bit, which cannot be set while
# building the package as an ordinary user.
if [ -e /opt/Rezoagwe/chrome-sandbox ]; then
    chown root:root /opt/Rezoagwe/chrome-sandbox || true
    chmod 4755 /opt/Rezoagwe/chrome-sandbox || true
fi
if command -v update-desktop-database >/dev/null 2>&1; then
    update-desktop-database -q /usr/share/applications || true
fi
if command -v gtk-update-icon-cache >/dev/null 2>&1; then
    gtk-update-icon-cache -q -t -f /usr/share/icons/hicolor || true
fi
EOF

cat > "$STAGE/DEBIAN/postrm" <<'EOF'
#!/bin/sh
set -e
if command -v update-desktop-database >/dev/null 2>&1; then
    update-desktop-database -q /usr/share/applications || true
fi
EOF

chmod 0755 "$STAGE/DEBIAN/postinst" "$STAGE/DEBIAN/postrm"
cp README.md "$STAGE/usr/share/doc/$PKG_NAME/README.md" 2>/dev/null || true

dpkg-deb --build -Z gzip --root-owner-group "$STAGE"
rm -rf "$STAGE"
echo ">> ${STAGE}.deb"
