#!/usr/bin/env node
// Draws the application icon: build/icon.png plus a sized set in build/icons/.
//
// The icon is generated rather than committed as a binary: it is the cluster
// graph the app draws — this node in the middle, peers on a ring — so it stays
// in step with the thing it stands for, and a packaging build needs no image
// editor and no ImageMagick.
//
// The PNG is written by hand (zlib + CRC32) so the whole build depends on
// nothing but Node.

import fs from 'node:fs';
import path from 'node:path';
import zlib from 'node:zlib';
import { fileURLToPath } from 'node:url';

const BUILD = path.join(path.dirname(fileURLToPath(import.meta.url)), '..', 'build');

/**
 * The sizes a desktop actually asks for.
 *
 * They are rendered rather than scaled from one master: electron-builder reads
 * the dimensions out of the file, and a single hand-written PNG left the icon
 * installed under `hicolor/0x0/apps` — which no menu ever looks in.
 */
const SIZES = [16, 24, 32, 48, 64, 128, 256, 512];

// The palette is the app's: a dark console ground, a green self node, and the
// per-peer colours the graph actually uses.
const BG = [11, 16, 21, 255];
const SELF = [0, 179, 126, 255];
const PEERS = [[79, 195, 247, 255], [255, 176, 32, 255], [129, 199, 132, 255], [186, 104, 200, 255]];
const LINK = [43, 58, 72, 255];

function render(SIZE) {
  const pixels = Buffer.alloc(SIZE * SIZE * 4);

  function put(x, y, [r, g, b, a]) {
    if (x < 0 || y < 0 || x >= SIZE || y >= SIZE) return;
    const i = (y * SIZE + x) * 4;
    if (a === 255) {
      pixels[i] = r;
      pixels[i + 1] = g;
      pixels[i + 2] = b;
      pixels[i + 3] = 255;
      return;
    }
    // Ordinary source-over blend, so an antialiased edge sits on what is under it.
    const alpha = a / 255;
    pixels[i] = Math.round(r * alpha + pixels[i] * (1 - alpha));
    pixels[i + 1] = Math.round(g * alpha + pixels[i + 1] * (1 - alpha));
    pixels[i + 2] = Math.round(b * alpha + pixels[i + 2] * (1 - alpha));
    pixels[i + 3] = Math.max(pixels[i + 3], a);
  }

  /** A filled circle with a one-pixel soft edge, so a 512px icon does not look cut out. */
  function disc(cx, cy, radius, colour) {
    for (let y = Math.floor(cy - radius - 1); y <= Math.ceil(cy + radius + 1); y++) {
      for (let x = Math.floor(cx - radius - 1); x <= Math.ceil(cx + radius + 1); x++) {
        const d = Math.hypot(x + 0.5 - cx, y + 0.5 - cy);
        if (d <= radius - 0.5) put(x, y, colour);
        else if (d < radius + 0.5) put(x, y, [colour[0], colour[1], colour[2], Math.round(255 * (radius + 0.5 - d))]);
      }
    }
  }

  function line(x0, y0, x1, y1, width, colour) {
    const steps = Math.ceil(Math.hypot(x1 - x0, y1 - y0) * 2);
    for (let i = 0; i <= steps; i++) {
      const t = i / steps;
      disc(x0 + (x1 - x0) * t, y0 + (y1 - y0) * t, width / 2, colour);
    }
  }

  /** A rounded square ground, which is what every desktop expects an icon to sit on. */
  function ground() {
    const radius = SIZE * 0.22;
    for (let y = 0; y < SIZE; y++) {
      for (let x = 0; x < SIZE; x++) {
        const dx = Math.max(radius - x, 0, x - (SIZE - radius - 1));
        const dy = Math.max(radius - y, 0, y - (SIZE - radius - 1));
        const d = Math.hypot(dx, dy);
        if (d <= radius - 0.5) put(x, y, BG);
        else if (d < radius + 0.5) put(x, y, [BG[0], BG[1], BG[2], Math.round(255 * (radius + 0.5 - d))]);
      }
    }
  }

  ground();

  const centre = SIZE / 2;
  const ring = SIZE * 0.28;
  const peers = PEERS.map((colour, i) => {
    const angle = -Math.PI / 2 + (2 * Math.PI * i) / PEERS.length;
    return { x: centre + ring * Math.cos(angle), y: centre + ring * Math.sin(angle), colour };
  });

  // Links first, so a node always sits on top of its own edges.
  for (const peer of peers) line(centre, centre, peer.x, peer.y, SIZE * 0.016, LINK);
  // One link between two peers: the thing a peer list cannot show and the graph can.
  line(peers[0].x, peers[0].y, peers[1].x, peers[1].y, SIZE * 0.012, LINK);

  for (const peer of peers) disc(peer.x, peer.y, SIZE * 0.055, peer.colour);
  disc(centre, centre, SIZE * 0.085, SELF);

  // ---- PNG encoding ---------------------------------------------------------

  const CRC_TABLE = (() => {
    const table = new Int32Array(256);
    for (let n = 0; n < 256; n++) {
      let c = n;
      for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
      table[n] = c;
    }
    return table;
  })();

  function crc32(buf) {
    let c = 0xffffffff;
    for (const byte of buf) c = CRC_TABLE[(c ^ byte) & 0xff] ^ (c >>> 8);
    return (c ^ 0xffffffff) >>> 0;
  }

  function chunk(type, data) {
    const length = Buffer.alloc(4);
    length.writeUInt32BE(data.length);
    const body = Buffer.concat([Buffer.from(type, 'ascii'), data]);
    const crc = Buffer.alloc(4);
    crc.writeUInt32BE(crc32(body));
    return Buffer.concat([length, body, crc]);
  }

  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(SIZE, 0);
  ihdr.writeUInt32BE(SIZE, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 6; // truecolour with alpha
  // compression, filter and interlace methods are all the only defined value, 0.

  // Each scanline is prefixed with its filter type; 0 (none) keeps this simple and
  // still compresses well on flat colour.
  const raw = Buffer.alloc((SIZE * 4 + 1) * SIZE);
  for (let y = 0; y < SIZE; y++) {
    raw[y * (SIZE * 4 + 1)] = 0;
    pixels.copy(raw, y * (SIZE * 4 + 1) + 1, y * SIZE * 4, (y + 1) * SIZE * 4);
  }

  const png = Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', ihdr),
    chunk('IDAT', zlib.deflateSync(raw, { level: 9 })),
    chunk('IEND', Buffer.alloc(0)),
  ]);

  return png;
}

fs.mkdirSync(path.join(BUILD, 'icons'), { recursive: true });
for (const size of SIZES) {
  fs.writeFileSync(path.join(BUILD, 'icons', `${size}x${size}.png`), render(size));
}
// The single file is what the hand-rolled deb script installs, and what any
// tool that wants one icon rather than a set will reach for.
const master = render(512);
fs.writeFileSync(path.join(BUILD, 'icon.png'), master);
console.log(`wrote ${BUILD}/icon.png and ${SIZES.length} sizes into ${BUILD}/icons/`);
