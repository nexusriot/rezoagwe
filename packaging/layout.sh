#!/bin/sh
# Lay out everything in a rezoagwe .deb that is not a binary: the systemd units,
# their environment files, and the docs.
#
# The Makefile and build-deb.sh both call this. They used to each build a
# package tree of their own, which is how one of them can ship a unit file the
# other does not.
set -e

root="${1:?usage: layout.sh <package-root>}"
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(dirname "$here")"

install -d -m 0755 "$root/lib/systemd/system"
install -m 0644 "$here/systemd/rezoagwe-bootstrap.service" "$root/lib/systemd/system/"
install -m 0644 "$here/systemd/rezoagwe-discovery.service" "$root/lib/systemd/system/"

install -d -m 0755 "$root/etc/default"
install -m 0644 "$here/default/rezoagwe-bootstrap" "$root/etc/default/"
install -m 0644 "$here/default/rezoagwe-discovery" "$root/etc/default/"

install -d -m 0755 "$root/usr/share/doc/rezoagwe"
install -m 0644 "$repo/README.md" "$root/usr/share/doc/rezoagwe/"
install -m 0644 "$repo/DESIGN.md" "$root/usr/share/doc/rezoagwe/"
