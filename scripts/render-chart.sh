#!/usr/bin/env bash
#
# Render rezo_agwe.png from rezo_agwe.drawio.
#
# The PNG is the picture the README shows, and it is the one document nothing
# else in this repository checks: a committed image keeps claiming whatever it
# claimed the day it was exported. The previous one said "protobuf", "no auth"
# and "no anti-entropy" for four releases after all three stopped being true,
# because re-exporting it meant opening a GUI. This makes it one command.
#
#   ./scripts/render-chart.sh          # rezo_agwe.drawio -> rezo_agwe.png
#   SCALE=1 ./scripts/render-chart.sh  # smaller, for a quick look
#
# Needs the draw.io desktop app on the PATH. It is an Electron program, so on a
# headless box run it under xvfb-run.

set -euo pipefail

cd "$(dirname "$0")/.."

src=rezo_agwe.drawio
out=rezo_agwe.png
scale="${SCALE:-2}"

command -v drawio >/dev/null || {
    echo "drawio is not on the PATH: https://www.drawio.com/blog/diagrams-offline" >&2
    exit 1
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

drawio -x -f png -s "$scale" --border 10 -o "$tmp/chart.png" "$src" \
    --no-sandbox --disable-gpu

# Flat fills and text quantise to a palette with no visible loss, and the file
# is a third of the size — which matters for something a README loads inline.
# Dithering is off: it speckles the large flat panels.
if command -v convert >/dev/null; then
    convert "$tmp/chart.png" -dither None -colors 256 "PNG8:$out"
else
    echo "ImageMagick not found; writing the unquantised export" >&2
    cp "$tmp/chart.png" "$out"
fi

echo "$src -> $out ($(du -h "$out" | cut -f1))"
