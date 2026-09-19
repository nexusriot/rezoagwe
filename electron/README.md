# R3zo Agwe for the desktop

The rezoagwe node **and** the rendezvous service in an Electron app, speaking the
same wire protocol as the Go implementation and the Android app.

It is a peer, not a viewer: the store, the replication, the chat and the
anti-entropy all run in this process, so the app is a member of the cluster
rather than a window onto someone else's node.

## What it does

* **Keys** — the KV store: create, edit and delete, with an optional TTL and a
  *Guard* checkbox that turns the save into a compare-and-swap against the
  version that was on screen. Each row shows its version and, for a key with an
  expiry, the time it has left. Per-key version history is one click away, and a
  refused guarded write says so on the screen you are looking at rather than only
  in the chat log.
* **Chat** — messages, `/me` emotes, direct messages, and the same slash commands
  the terminal client has (`/help` lists them).
* **Peers** — who is in the cluster, with nicknames and last-seen ages, plus the
  start/stop control for the node itself. *Sync* pulls a peer's store, *Gossip*
  sends it our membership view and a digest now, *Forget* drops the link on
  purpose so the cluster can be watched healing.
* **Graph** — the cluster drawn as a graph: this node in the middle, its peers on
  the first ring, and nodes it only knows from gossip further out. Links both
  ends confirm are solid, links only one end claims are dashed. A peer list
  cannot show that two of your peers do not talk to each other, or that the
  cluster has split in two; both are visible here. Scroll to zoom, drag to pan,
  click a node for its details.
* **Activity** — replication as it happens: what was applied, what was rejected
  as stale, what anti-entropy pushed or pulled — alongside the counters. All of
  it is otherwise invisible, which is when a replication bug hides.
* **Diag** — the whole node in one screen, starting with what is wrong. Health
  checks turn counters into causes ("packets failed authentication" → the key or
  cluster name differs, with the key fingerprint to compare); then identity,
  sockets, traffic per peer, addresses that send packets without being peers,
  store shape and the topology. *Copy report* puts all of it on the clipboard.
* **Bootstrap** — run the rendezvous service here, so this machine is the meeting
  point a LAN cluster forms around. It never sees KV data or chat.
* **Settings** — nickname, port, advertised host, bootstrap seeds, cluster name,
  pre-shared key, tombstone GC age, and what starts on launch.

The window splits in two when it is wide enough, with the pairing chosen so the
second pane answers what the first raises: peers next to the graph, chat next to
peers, settings next to diagnostics. The split collapses from the header.

## Build and run

```
make install               # npm install (downloads Electron)
make run                   # run it            (make dev opens devtools)
make test                  # the unit suite: a whole cluster in one process
make check                 # syntax + shape check over every source file
make selftest              # boot the real window and drive every screen
make verify                # the suite, the window, and a packaged build's own selftest
```

`make help` lists everything. Every target has an npm script behind it
(`npm test`, `npm run selftest`, …) if you would rather call those directly.

## Packaging

```
make binary                # unpacked, runnable    -> dist/linux-unpacked/
make tarball               # portable .tar.gz      -> dist/
make deb                   # .deb for this machine -> dist/
make appimage              # portable AppImage     -> dist/
make dist                  # deb + AppImage
make debs                  # .debs for amd64 + arm64 + armhf
make deb-manual            # a .deb with dpkg-deb alone: no fpm, no network
```

The version comes from `package.json` and can be overridden for a one-off build
— `make deb VERSION=0.9.0-rc1` stamps it into the artifact *and* into the app, so
what the file is called and what the app reports cannot drift apart.

Two packaging paths exist on purpose, because they fail in different places:

* **`make deb`** uses electron-builder, which wants `fpm` (it downloads one into
  `~/.cache/electron-builder` the first time) and can cross-build for arm64 and
  armhf by fetching those Electron binaries.
* **`make deb-manual`** lays out an unpacked build by hand and packs it with
  `dpkg-deb`, which any Debian box already has. It is the path that still works
  on a machine with no network and no fpm.

Both produce the same package: `rezoagwe-desktop`, installed under
`/opt/Rezoagwe` with a launcher at `/usr/bin/rezoagwe-desktop`, a desktop entry
and the icon at every size a menu looks for. A test asserts the two descriptions
agree, since the failure mode is a half-removed installation rather than a build
error.

The icon is generated (`make icon`) rather than committed: it is the cluster
graph the app draws, and the generator is a PNG encoder in plain Node, so
packaging needs no image editor and no ImageMagick.

From the repository root: `make electron` runs it, `make electron-test` runs the
suite, `make electron-deb` / `electron-debs` / `electron-appimage` / `electron-dist`
package it, and `make electron-verify` does the lot.

## Talking to a Go cluster

Point the app at a bootstrap seed reachable from this machine, and give it the
same cluster name and pre-shared key:

```
rezoagwe-bootstrap -headless -port 9999 -psk demo -cluster home
rezoagwe-discovery -headless -node 192.168.1.10:3137 -bootstrap 192.168.1.10:9999 \
    -psk demo -cluster home -http 127.0.0.1:8080
```

Then in **Settings**: seeds `192.168.1.10:9999`, cluster `home`, key `demo`.
Leave *Advertise host* blank and the app uses its own LAN address, which is what
peers need to reach it.

Nodes only talk to peers with the same cluster **and** key. Without a key the
framing key comes from the cluster name alone: that separates two clusters
sharing a network, but provides no secrecy.

## Layout

```
electron/
├── src/
│   ├── main.js            window, menu, IPC, --selftest and --screenshot
│   ├── preload.js         the whole renderer-visible API, one explicit bridge
│   ├── proto/
│   │   ├── wire.js        message bodies, field-for-field with the Go structs
│   │   └── codec.js       framing, HMAC authentication, replay guard
│   ├── net/
│   │   ├── udp.js         datagrams + streams on one socket
│   │   ├── mem.js         in-process network with loss, delay and partitions
│   │   ├── framing.js     length-prefixed stream frames
│   │   └── addr.js        address parsing, validation, normalisation
│   └── core/
│       ├── kvstore.js         versioned store: LWW, CAS, TTL, digests, GC
│       ├── node-engine.js     membership, replication, chat, anti-entropy
│       ├── bootstrap-server.js the rendezvous role
│       ├── topology.js        the cluster graph, derived from gossiped peer lists
│       ├── diagnostics.js     one snapshot of the node, plus the health checks
│       ├── metrics.js         counters, per-peer traffic and rates
│       ├── persistence.js     atomic, generation-guarded state file
│       ├── settings.js        what the user can configure, and what needs a restart
│       └── runtime.js         owns both roles for the process
├── renderer/               the UI: no build step, no framework, no inline script
├── scripts/
│   ├── check.js            syntax + shape check, in place of a linter
│   └── make-icon.mjs       the app icon, drawn and PNG-encoded in plain Node
├── build-deb.sh            the fpm-free packaging path
└── Makefile                build, test and packaging targets
```

The engine half depends on nothing from Electron, which is what lets a whole
cluster run inside the test process.

## Parity with the Go implementation

The app shares no code with the Go node — only the protocol — so the rules that
matter (framing, last-write-wins, digest ranges, version-preserving expiry) are
duplicated deliberately and pinned by tests:

* `codec.test.js` decodes a frame produced by the **Go** codec and checks the
  derived keys against fixed vectors from `proto.DeriveKey`.
* `wire.test.js` decodes the bodies Go actually emits. Go marshals a nil slice as
  `null` for any field without `omitempty`, so an empty Go node's digest arrives
  as `{"from":"…","entries":null}` — which authenticates and would then fail to
  parse, silently dropping the repair it carried.
* `kvstore.test.js` asserts the same merge, CAS, TTL and reconciliation rules the
  Go store's tests assert — including the store **fingerprint** (wire kinds 11
  and 12), whose XOR-over-buckets fold has to produce byte-identical digests to
  the Go one or a consistency check reports two converged replicas as divergent.
* `cluster.test.js` runs a whole cluster in one process over an in-memory network
  with configurable loss and partitions: convergence under 30 % packet loss, a
  dropped write repaired by a digest exchange, a delete that is not resurrected,
  a joiner that inherits a store nobody replayed to it.
* `wiring.test.js` checks the seams a unit test cannot see: every channel the
  preload exposes has a handler behind it, the window is built with the
  isolation settings that make that bridge safe, and nothing in the page relies
  on an inline script or style the CSP would drop in silence.
* `packaging.test.js` holds the two packaging paths to the same description, so
  a deb from either is the same package.
* `docs.test.js` checks the documentation the way the rest is checked: every
  `make` target and `npm run` script the docs offer exists, every link and every
  file named in a layout tree is there, and the tables people read instead of
  the source — the message kinds, the HTTP routes, the chat commands, the TUI
  hotkeys, the CLI flags — still match the code they describe.
* `interop.test.js` builds the real Go binaries, runs a real rendezvous and a
  real peer, and replicates through them in both directions. It is off by
  default because it compiles Go:

```
REZOAGWE_GO_INTEROP=1 npm run test:interop
```

## Verifying a UI change

`npm run selftest` boots the real window, walks every screen, writes a key and
checks it lands, draws the graph, opens the split view and runs a slash command —
then exits non-zero on anything it could not prove. It catches what unit tests
structurally cannot: a preload broken by a sandbox change, a CSP that blocks the
scripts, a screen that throws before its first paint.

`electron . --screenshot <dir>` writes a PNG of every screen. It captures through
the window's own compositor, so it works on a machine whose screen is locked or
which has no desktop at all.

## Notes

* The renderer is sandboxed with context isolation on and a strict CSP: no
  inline scripts, no inline styles, no `innerHTML`. Values here come off the
  network — a key, a nickname, a chat line is whatever a peer sent — so they are
  only ever assigned as text.
* There is no tray icon, so on Linux and Windows closing the window quits the
  app and stops the node. That is deliberate rather than hidden: quitting first
  announces a `Goodbye`, so peers drop this node at once instead of waiting out
  the eviction timeout. On macOS the app follows the platform convention and
  stays alive with its window closed. A tray icon and a close-to-tray setting
  are item 6 in [the roadmap](../ROADMAP.md).
* Settings are kept in the Electron user-data directory
  (`~/.config/rezoagwe-desktop/settings.json` on Linux), next to the node's own
  state file. Changing the port, cluster or key restarts whichever role is
  running, because those are baked into a bound socket and a derived key.
