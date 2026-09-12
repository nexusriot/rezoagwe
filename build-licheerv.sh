#!/bin/env bash
# Build the rezoagwe binaries for the LicheeRV Nano (W) — linux/riscv64.
#
#   ./build-licheerv.sh           -> bare static binaries into dist/
#   ./build-licheerv.sh deb       -> riscv64 .deb into build/
set -e

# One source of truth for the version: the Makefile. Hardcoding it here is how
# this script came to build 0.0.3 packages long after the Makefile had moved on.
version="${VERSION:-$(sed -n 's/^VERSION[[:space:]]*?*=[[:space:]]*//p' "$(dirname "$0")/Makefile" | head -1)}"
version="${version:-0.0.0}"

if [ "${1:-}" = "deb" ]; then
  dir="$(cd "$(dirname "$0")" && pwd)"
  exec "$dir/build-deb.sh" riscv64
fi

dist_dir="dist"
mkdir -p "$dist_dir"

echo "building rezoagwe $version for LicheeRV Nano (linux/riscv64)"

CGO_ENABLED=0 GOOS=linux GOARCH=riscv64 \
  go build -trimpath -ldflags "-s -w" \
  -o "$dist_dir/rezoagwe-bootstrap-${version}-licheerv-linux-riscv64" ./cmd/bootstrap
echo ">> $dist_dir/rezoagwe-bootstrap-${version}-licheerv-linux-riscv64"

CGO_ENABLED=0 GOOS=linux GOARCH=riscv64 \
  go build -trimpath -ldflags "-s -w" \
  -o "$dist_dir/rezoagwe-discovery-${version}-licheerv-linux-riscv64" ./cmd/discovery
echo ">> $dist_dir/rezoagwe-discovery-${version}-licheerv-linux-riscv64"
