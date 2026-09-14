#!/bin/env bash
set -e

# One source of truth for the version: the Makefile. Hardcoding it here is how
# this script came to build 0.0.3 packages long after the Makefile had moved on.
version="${VERSION:-$(sed -n 's/^VERSION[[:space:]]*?*=[[:space:]]*//p' "$(dirname "$0")/Makefile" | head -1)}"
version="${version:-0.0.0}"
arch="${1:-amd64}"

echo "building deb for rezoagwe $version ($arch)"

if ! type "dpkg-deb" > /dev/null; then
  echo "please install required build tools first (dpkg-deb)"
  exit 1
fi

case "$arch" in
  amd64)  goarch="amd64"; goarm="" ;;
  i386)   goarch="386";   goarm="" ;;
  arm64)  goarch="arm64"; goarm="" ;;
  armhf)   goarch="arm";     goarm="7" ;;
  riscv64) goarch="riscv64"; goarm="" ;;
  *)      echo "unsupported architecture: $arch"; exit 1 ;;
esac

# The same ldflags the Makefile uses. Without -X, the binaries inside a package
# stamped with a version report "dev" instead — the same drift the
# version-from-the-Makefile lookup above exists to prevent, one layer down.
ldflags="-s -w -X main.version=$version"

project="rezoagwe_${version}_${arch}"
folder_name="build/$project"
echo "creating $folder_name"
rm -rf "$folder_name"
mkdir -p "$folder_name"
cp -r DEBIAN/ "$folder_name/"
bin_dir="$folder_name/usr/bin"
mkdir -p "$bin_dir"

CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" ${goarm:+GOARM=$goarm} \
  go build -trimpath -ldflags "$ldflags" -o "$bin_dir/rezoagwe-bootstrap" ./cmd/bootstrap
CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" ${goarm:+GOARM=$goarm} \
  go build -trimpath -ldflags "$ldflags" -o "$bin_dir/rezoagwe-discovery" ./cmd/discovery
chmod 0755 "$bin_dir/rezoagwe-bootstrap" "$bin_dir/rezoagwe-discovery"

./packaging/layout.sh "$folder_name"

sed -i "s/_version_/$version/g" "$folder_name/DEBIAN/control"
sed -i "s/^Architecture: .*/Architecture: $arch/" "$folder_name/DEBIAN/control"

cd build/ && dpkg-deb --build -Z gzip --root-owner-group "$project"
echo ">> build/$project.deb"
