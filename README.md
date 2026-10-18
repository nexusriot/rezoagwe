## R3zo Agwe

_Distributed key-value store with an embedded chat, written in Go —
with Android and desktop apps that speak the same protocol._

Three binaries:

* **`rezoagwe-bootstrap`** — rendezvous service. Knows only the *set of
  node addresses*; never sees KV data or chat.
* **`rezoagwe-discovery`** — full peer node. Owns a local KV replica,
  participates in chat, gossips peer membership, reconciles divergence
  with anti-entropy, and runs a `tview` TUI (or none, with `-headless`).
* **`rezoagwectl`** — a client, not a peer. Drives a running node through its
  HTTP gateway: it holds no replica, joins no cluster and appears in nobody's
  peer list. Looking at a cluster should not mean becoming a member of it.

A new node contacts bootstrap **once at startup** to learn the initial
roster. From then on, peers gossip among themselves.

For internals, wire protocol, and the failure model, see
[DESIGN.md](DESIGN.md).

### **Why?**

![Pic](https://github.com/nexusriot/rezoagwe/blob/main/wtf.png)

### **Design (concept)**

![Pic](https://github.com/nexusriot/rezoagwe/blob/main/rezo_agwe.png)

The picture is exported from [rezo_agwe.drawio](rezo_agwe.drawio) — edit that,
then `make chart`. A test fails if the two drift apart, or if the drawing starts
claiming something the protocol stopped doing.

---

### Features

- Eventually-consistent distributed KV with **per-key last-write-wins**:
  every write carries a version (Lamport counter + node id), so concurrent
  writes to the same key converge to the same value on every node, and
  deletes are versioned tombstones that a stale write can't resurrect
- **Anti-entropy**: every gossip tick exchanges a digest of a slice of the
  keyspace with one peer and repairs whatever is missing or stale in either
  direction — so a write whose packet was dropped is re-sent instead of
  leaving two stores divergent forever
- **Compare-and-swap**: a write or delete can be guarded by the version it
  expects, and a zero version means "only if absent" — enough to build a
  lock or a leader election on
- **Key TTL**: an optional expiry per key, applied deterministically on
  every replica without replicating anything, plus opt-in tombstone GC
- **Authenticated wire**: every packet — including bootstrap traffic —
  carries an HMAC over a cluster-derived key, with a nonce and timestamp
  that make replays and cross-cluster crosstalk fail closed
- **State sync over TCP**: a joining node pulls the whole store and recent
  chat over a stream, so a store larger than a datagram syncs correctly;
  the datagram path remains as a fallback
- Local persistence: identity, values, versions, Lamport clock, tombstones
  and chat are written atomically and reloaded on startup (`-data`, default
  `<config dir>/rezoagwe/<node>.json`; `-data -` disables). Writes are
  coalesced — a burst costs one rewrite, not one per entry — and a clean
  shutdown flushes. Node identity survives a restart, so a node keeps its
  place in the version ordering even if it moves to another port
- **HTTP gateway** (`-http`): `GET/PUT/DELETE /kv/{key}` with `If-Match`
  compare-and-swap, `If-None-Match` conditional reads and TTL headers, plus
  history, peers, chat, activity, export/import, health and Prometheus
  metrics — behind an optional bearer token (`-http-token`), TLS
  (`-http-tls-cert`/`-http-tls-key`) and a read-only mode
  (`-http-readonly`)
- **Cluster graph** (`g`, `GET /topology`): who this node can see, who its
  peers claim to see, which links only one end reports, and whether the
  known nodes fall into more than one component — which is what a partition
  looks like from the inside
- **Diagnostics** (`D`, `GET /diagnostics`, `-doctor`): the counters turned
  into causes. A key mismatch, a clock out of skew, one-way UDP through a
  firewall, a loopback address advertised to a LAN, peers about to be
  evicted — each named, with what to do about it
- **Consistency check** (`v`, `GET /consistency`, *Verify replicas* on
  Android): asks every peer to summarise its whole store and reports who
  disagrees, about which region of the keyspace, and which keys. Anti-entropy
  repairs divergence but never reports it, so a cluster can sit split for as
  long as nobody looks. The summary covers each entry's key, version, whether
  it is deleted **and a hash of its value**, so two replicas holding different
  data at the same version — what two nodes started from a copied state file
  produce, and the one split replication can never repair — are reported rather
  than mistaken for agreement. All three implementations answer and all three
  can ask; their bucket digests are pinned to each other byte for byte
- **`-advertise`**: what peers are told, separate from what the node binds.
  A node bound to `:3137` otherwise tells every peer to reach it at
  `:3137`, which each of them resolves to its own loopback
- **Store limits**: `-max-value-bytes` and `-max-keys` bound what a node
  will hold, from a local writer or a peer — and a peer's update that is
  refused says so, by name and by reason, instead of being filed as stale
- **Replication metrics and an activity feed**: remote applies, stale
  rejections, writes refused by this node's limits, refused guarded writes
  and repair traffic are visible rather than silent — as counters, per-peer
  traffic in both directions, and gauges for what the store holds right
  now. A refusal is counted apart from a stale rejection and raised as a
  diagnostics error, because the two mean opposite things: one is
  last-write-wins working, the other is a replica that cannot converge
  until a limit moves
- **Nothing is stored that cannot be replicated intact**: bodies are JSON, and
  every JSON encoder silently replaces a byte that is not valid UTF-8 with
  `U+FFFD` — so a binary value used to be held intact by the node that wrote it
  and mangled on every replica, *at the same version*, which anti-entropy is
  right to call agreement and can therefore never repair. A key fared worse: it
  became a different key. Such a write is now refused (`422`) with the
  workaround named, the way an unreplicably large value already was. Everything
  valid still crosses the wire byte for byte — control characters, quotes,
  newlines, emoji
- **Repair** (`POST /repair`, `rezoagwectl repair`): the consistency check
  named the keys replicas disagreed on and stopped there. This is the cure —
  it makes one node's copy authoritative by re-stamping it, so last-write-wins
  carries it everywhere. Which node is a judgement about the data that nothing
  in the protocol can make, so it is an explicit operator action
- **`-advertise auto`**: the rendezvous service is the only component that sees
  where a node's packets really come from, and it used that address only to
  pick a reply path. It now reports it, so a node can be told "you advertise
  `:3137`, the cluster sees you at `10.0.0.4`" — with the exact flag value to
  use — instead of being guessed at by a loopback heuristic. `auto` adopts it
- **Watch**: `GET /kv?watch=1` streams every mutation as server-sent events —
  local writes, updates merged from peers, and TTL expiries, which replicate
  nothing and so could not be noticed any other way. The stream opens with the
  store as it stands, taken *after* the subscription, so a write racing the
  call is delivered rather than lost. A subscriber that falls behind is told how
  many events it missed rather than quietly skipping them
- **LAN discovery** (`-mdns`): find peers with no seed configured at all. The
  node advertises itself over multicast DNS and adopts what answers in its own
  cluster. It also stops advertising `:3137` to a LAN, which is the single most
  common reason a cluster never forms
- **Chat relay**: a broadcast reaches only the peers its *author* knew, and peer
  tables are routinely asymmetric. A node that can see, from gossip, that the
  author could not reach one of its own peers forwards the line there — but only
  on that evidence, so a healthy mesh forwards nothing
- **Scheduled snapshots** (`-snapshot-dir`): the state file is a replica, not a
  backup — it is rewritten in place on every flush, so a key deleted by mistake
  is gone from it within 250 ms. Periodic timestamped exports with rotation are
  the backup, and `POST /snapshot` takes one before a risky change
- **Timings, not just totals**: histograms for the gossip round, state sync and
  stream exchanges, plus how late each gossip tick actually fired. A node whose
  loop falls behind its peers' eviction threshold is evicted while working
  perfectly, and every counter stays clean while it happens — the diagnostics
  now name it
- **Key history**: the recorded versions of a key, and who wrote each
- Robust join: an unreachable or lossy bootstrap can't stall startup, and
  the node keeps retrying while it knows no peers; several seeds can be
  given as `-bootstrap a,b,c`
- Peer membership by periodic gossip + Hello replies, graceful `Goodbye` on
  quit, and stale-peer eviction
- Built-in chat with nicknames, `/me` emotes, direct messages, per-sender
  colours and slash commands shared by every front end
- **Chat delivery you can check**: every broadcast carries its author's own
  sequence number, and every heartbeat says where that author's numbering
  stands — so a line that never arrived is *named* rather than silently
  absent, even from a peer that has since gone quiet. It is then asked for
  from the author, re-inserted where it was said, and recognised as a
  re-delivery instead of shown twice; a line the author can no longer supply
  is reported as lost rather than pending forever. The repair channel can only
  ever *fill a gap that was asked for*, never introduce a line. Chat has no
  digest behind it the way the store does, so before this a dropped line was
  not quiet, it was invisible: the sender's log said sent and no counter
  anywhere moved
- Per-node TUI on `tview`/`tcell`: keys list with filter and expiry
  markers, details, peer list, a feed pane that toggles between chat and
  replication activity, modals for history, metrics, help and
  import/export, and a status bar
- **Android app** (`android/`): the same node *and* the rendezvous service
  on a phone or tablet, with a Compose UI, a cluster graph, a diagnostics
  screen, and a foreground service
- **Desktop app** (`electron/`): the same two roles on a workstation, with the
  cluster drawn as a graph, a diagnostics screen that turns counters into
  causes, and a split view — a full peer, not a viewer onto someone else's node
- Cross-build to Linux (amd64/i386/arm64/armv7/riscv64), FreeBSD, macOS,
  Windows; Debian packages with systemd units for both roles
- CI on every push: the Go suite under the race detector, every cross-build
  target, the desktop suite, the Android unit tests, and the containerised
  end-to-end run

---

### Build

```
make x86_64           # linux/amd64, dynamic
make x86_64-static    # linux/amd64, fully static (CGO off)
make all              # every cross-build target into dist/
make debs             # .debs: amd64 + i386 + arm64 + armhf + riscv64
make android          # debug APK into dist/
make electron         # run the desktop client
make electron-test    # its unit suite (a whole cluster in one process)
make test-race        # the full test suite under the race detector
make bench            # benchmarks for the replication primitives
make fuzz             # fuzz the wire codec (FUZZTIME=5m for longer)
make e2e              # hermetic multi-node run in Docker
make help             # full list of targets
```

### End-to-end tests

`make e2e` builds the real binaries into an image and runs a cluster of
containers on a private network — a rendezvous, three peers, a node that
arrives after the store already has content, one that shuts down mid-run, one
that restarts with its state file intact, one configured tighter than the rest
so the cluster has something to disagree about, one alone in its own cluster
with limits small enough to trip in a single request, and two strangers that
share the network but differ in the key or the cluster name. A Go suite in a
further container drives all of it through the HTTP gateway, including a watch
stream held open against one node while another is written to — which is the
one route whose behaviour does not exist inside a single process. Nothing is published to
the host, no node persists anything outside its container, and the stack comes
down whether the run passes or fails.

```
make e2e                          # the whole thing, about a minute
KEEP_STACK=1 make e2e             # leave it up afterwards to poke at
E2E_LATE_JOIN_SEC=10 make e2e     # move the timed events around
```

The unit tests run a whole cluster inside one process over an in-memory
network, which is the only way to test a partition deterministically — and the
reason they cannot see anything that goes wrong *between* processes. The
end-to-end suite is that half: real sockets, real framing, real timing, and
assertions on the things that only exist between nodes — that a write reaches a
node it was not sent to, that a stale compare-and-swap is refused everywhere
rather than just locally, that a joiner ends up with a store nobody replayed to
it, that a node which shuts down is *announced* rather than merely timed out,
and that a snapshot merged into history a node already had does not show the
conversation twice.

### Debian packages

A single `.deb` installs `/usr/bin/rezoagwe-bootstrap`,
`/usr/bin/rezoagwe-discovery` and `/usr/bin/rezoagwectl`. `make debs` builds every architecture; to package
one directly:

```
./build-deb.sh amd64
./build-deb.sh i386
./build-deb.sh arm64      # or: ./build-deb-arm64.sh
./build-deb.sh armhf
./build-deb.sh riscv64    # or: ./build-licheerv.sh deb
```

The version comes from the Makefile, so the package and the binaries inside it
always agree; `VERSION=0.9.0-rc1 ./build-deb.sh amd64` overrides both.

The package also installs systemd units for both roles, their
`/etc/default` files, and a `rezoagwe` system user owning
`/var/lib/rezoagwe`. **Neither unit is enabled on install** — which role a
machine plays is a decision, and starting a rendezvous service nobody asked
for would put a listener on 9999 the moment the package lands:

```
sudoedit /etc/default/rezoagwe-discovery     # set -advertise, seeds, psk
sudo systemctl enable --now rezoagwe-discovery
```

Stopping a node through systemd announces its departure, so peers drop it at
once instead of waiting out the eviction timeout.

The desktop client is a separate package — `rezoagwe-desktop`, built from
`electron/` — so the two install side by side.

### Usage

```
./rezoagwe-bootstrap [-port number] [-psk key] [-cluster name] [-headless]
./rezoagwe-bootstrap -port 9999
```
default port is **9999** (UDP and TCP)

```
./rezoagwe-discovery [-bootstrap seeds] [-node addr] [-advertise addr] [-nick name]
                     [-data file] [-psk key] [-cluster name] [-http addr]
                     [-http-token tok] [-http-readonly] [-headless] [-doctor]
./rezoagwe-discovery -bootstrap :9999 -node :3137 -nick alice
./rezoagwe-discovery -bootstrap :9999 -node :3138 -nick bob
```

defaults: `-bootstrap :9999`, `-node :3137`, `-nick anon`,
`-cluster rezoagwe`. The second node pulls alice's store on join, both see
each other in the **Nodes** pane, chat flows both ways, and KV writes
propagate.

**On more than one machine, set `-advertise`.** `-node` is what the socket
binds; `-advertise` is what peers are told. A node bound to `:3137` tells
every peer to reach it at `:3137`, and each of them resolves that to its own
loopback:

```
./rezoagwe-discovery -bootstrap 10.0.0.9:9999 -node :3137 -advertise 10.0.0.4:3137
```

**On one LAN, `-mdns` replaces the seeds entirely.** The node advertises itself
over multicast DNS, adopts whatever answers in its own cluster, and — because
`-mdns` means "I am on a LAN" — stops advertising a bare `:3137` that every
peer would resolve to its own loopback:

```
./rezoagwe-discovery -mdns -bootstrap "" -nick alice
./rezoagwe-discovery -mdns -bootstrap "" -node :3138 -nick bob
```

It is a convenience, not a security boundary: the cluster name in the published
record is a hint, and a node still has to produce a valid MAC before anything
it says is believed. Multicast does not cross subnets and is often blocked on
guest Wi-Fi, so a cluster spanning networks still needs a rendezvous.

**Back the store up on a timer.** The state file is a replica, not a backup —
it is rewritten in place on every flush, so a key deleted by mistake is gone
from it within 250 ms:

```
./rezoagwe-discovery -snapshot-dir /var/lib/rezoagwe/snapshots \
    -snapshot-interval 1h -snapshot-keep 24
```

Snapshots are exports, so they carry versions: restoring one through
`POST /import` merges it under last-write-wins rather than forcing stale data
back over what the cluster has moved on to. `POST /import?mode=seed` is the
other choice, and it wins.

If a cluster will not form, ask the node why:

```
./rezoagwe-discovery -bootstrap 10.0.0.9:9999 -advertise 10.0.0.4:3137 -doctor
```

It starts, waits out two heartbeats, and prints what is wrong — a key or
cluster-name mismatch, a clock outside the skew window, one-way UDP, a
loopback address advertised to a LAN, peers that never answered.

Nodes only talk to peers with the same `-cluster` **and** `-psk`. Without a
`-psk` the framing key comes from the cluster name alone: that separates two
clusters sharing a network, but provides no secrecy.

The flags above are the ones a first run needs. For every flag all three
binaries accept — store limits, tombstone GC, TLS, logging — see
[DESIGN.md §15](DESIGN.md#15-command-line-reference).

### `rezoagwectl`

A client, not a peer. Every other front end here — the TUI, the phone, the
desktop app — joins the cluster to look at it: it gossips, holds a replica and
appears in everyone's peer list. `rezoagwectl` talks to a node's HTTP gateway
and nothing else.

```bash
export REZOAGWE_SERVER=http://127.0.0.1:8080   # and REZOAGWE_TOKEN if set
rezoagwectl put colour blue                    # prints the version it landed at
rezoagwectl get colour
printf 'a\nb\n' | rezoagwectl put notes       # a value from stdin
rezoagwectl -ttl 60s put session/abc token
rezoagwectl -if-match '*' put lock mine        # claim; exit 3 if someone has it
rezoagwectl ls app/
rezoagwectl watch app/                         # stream changes until Ctrl+C
rezoagwectl peers ; rezoagwectl doctor ; rezoagwectl check
rezoagwectl repair colour                      # make THIS node's copy win
rezoagwectl repair -divergent                  # ...for every key the check names
rezoagwectl export backup.json ; rezoagwectl -seed import backup.json
rezoagwectl say "deploying now" ; rezoagwectl chat
```

Exit codes are the point of the compare-and-swap commands: **0** ok, **1**
error, **2** no such key, **3** version mismatch, **4** replicas disagree. A
cron job can branch on `rezoagwectl check` without reading the report, and a
lock loop can tell "someone got there first" from "this will never work".

`-json` gives machine-readable output where there is a choice; `get` prints the
raw value with no trailing newline, so `$(rezoagwectl get k)` is the value.
Flags work before or after the command, and a key that begins with a dash goes
after `--`.

**`repair` is the one command here that can make a cluster worse.** It
overwrites the named keys on every replica with this node's copy. That is the
cure for a split anti-entropy cannot resolve — two replicas holding different
values at the same version — and a way to destroy good data if pointed at the
wrong node. See [DESIGN.md §14.2](DESIGN.md#142-repair-the-cure-the-check-never-had).

### Desktop app

```
make electron              # run it
make electron-test         # unit suite, no display needed
make electron-selftest     # boot the window and drive every screen
make electron-verify       # suite + window + a packaged binary's own selftest
make electron-binary       # unpacked build   -> electron/dist/linux-unpacked/
make electron-deb          # .deb             -> electron/dist/
make electron-debs         # .debs for amd64 + arm64 + armhf
make electron-appimage     # portable AppImage
make electron-dist         # deb + AppImage
```

The `.deb` installs as **`rezoagwe-desktop`** under `/opt/Rezoagwe`, with a
launcher on the PATH and a desktop entry, so it sits alongside the `rezoagwe`
package that carries the two Go binaries rather than colliding with it. On a
machine with no network and no `fpm`, `make -C electron deb-manual` builds the
same package with `dpkg-deb` alone.

Point it at a rendezvous service in **Settings** (seeds, cluster name and key),
and it joins as a peer: the store, the chat, the gossip and the anti-entropy all
run in the app. See [electron/README.md](electron/README.md).

### Hotkeys (discovery TUI)

| Key             | Action                                          |
|-----------------|-------------------------------------------------|
| `c`             | Create key (value, TTL, optional guard)         |
| `e` / `Enter`   | Edit key under cursor                           |
| `d`             | Delete key under cursor                         |
| `h`             | Version history of the key under cursor         |
| `/`             | Focus the filter; type to filter keys/values    |
| `Enter` (filter)| Back to the keys list, filter kept              |
| `Esc` (filter)  | Clear the filter and go back to the keys list   |
| `a`             | Switch the feed between chat and activity       |
| `m`             | Replication metrics                             |
| `g`             | Cluster graph: peers, links, partitions         |
| `D`             | Diagnostics: what is actually wrong             |
| `v`             | Verify every replica holds the same store       |
| `x` / `i`       | Export / import the store                       |
| `?`             | Help                                            |
| `Tab`           | Cycle focus: Keys → Nodes → Feed → Message      |
| `Enter` (chat)  | Send the chat message                           |
| `Ctrl+Q`        | Quit (announces departure to peers)             |

Tick **Guard** in the edit form to make the save a compare-and-swap against
the version that was on screen: if a peer wrote the key meanwhile, the save
is refused instead of overwriting it.

### Chat commands

`/nick <name>` · `/me <text>` · `/msg <peer> <text>` · `/peers` · `/keys` ·
`/get <key>` · `/set <key> <value>` · `/setttl <key> <seconds> <value>` ·
`/del <key>` · `/help`

### HTTP gateway

```
rezoagwe-discovery -headless -node 127.0.0.1:3137 -bootstrap 127.0.0.1:9999 -http 127.0.0.1:8080
```

```bash
curl -X PUT --data-binary 'blue' localhost:8080/kv/colour     # write
curl localhost:8080/kv/colour                                  # read
curl -X PUT --data-binary 'v' -H 'If-Match: *' localhost:8080/kv/lock   # claim
curl -X PUT --data-binary 'v' -H 'X-Rezoagwe-TTL: 60' localhost:8080/kv/session
curl localhost:8080/peers ; curl localhost:8080/health ; curl localhost:8080/metrics
curl localhost:8080/export > backup.json
curl -X POST --data-binary @backup.json 'localhost:8080/import?mode=seed'
curl localhost:8080/topology                    # the cluster as a graph
curl 'localhost:8080/diagnostics?format=text'   # what is wrong, in prose
curl localhost:8080/consistency                 # 409 if replicas disagree
curl -N 'localhost:8080/kv?watch=1'             # stream every change
curl -X POST localhost:8080/snapshot            # write a backup now
curl -X POST -d '{"keys":["colour"]}' localhost:8080/repair   # this node's copy wins
```

`?watch=1` turns `GET /kv` into a server-sent event stream: an opening
`snapshot` event with the store as it stands, then a `kv` event per mutation
carrying the update and where it came from (`local`, `remote` or `expiry`).
Add `&prefix=` to scope it. A subscriber that falls behind gets a `dropped`
event saying how many it missed, because a watch with a silent hole in it is
worse than no watch.

**The gateway is a write surface.** Anything that can reach the port can
rewrite the whole cluster through `POST /import?mode=seed`, so bind it to
loopback or lock it down:

```
./rezoagwe-discovery -headless -http :8080 -http-token "$TOKEN" \
    -http-tls-cert cert.pem -http-tls-key key.pem
```

```bash
curl -H "Authorization: Bearer $TOKEN" https://host:8080/kv/colour
```

The token is required on **every** route, `/health` and `/metrics`
included. `-http-readonly` refuses every mutating method, which is what
makes a gateway safe to point a dashboard or a scraper at.

See [DESIGN.md §8](DESIGN.md#8-http-gateway) for the whole surface.

### Android

`android/` builds an app that runs the node, the rendezvous service, or
both — with the KV store, peers, chat, activity feed, metrics, a cluster
graph and a diagnostics screen on screen, and a foreground service so the
node keeps gossiping when the screen is off. The layout adapts to the
window: tabs on a phone, a navigation rail on a tablet, and two screens
side by side when there is room for them.

```
cd android && ./gradlew :app:assembleDebug
```

It shares no code with the Go implementation, only the protocol, so parity
is pinned by tests: a frame produced by the Go codec is decoded by the
Kotlin one, and an opt-in live test joins a running Go cluster and
replicates through it in both directions. It has been run on a tablet against
a Go cluster on a real LAN — see [android/README.md](android/README.md) for what
that verified and the one bug it found.

### Status

Replication converges concurrent writes via per-key last-write-wins, repairs
dropped packets via anti-entropy, authenticates every packet, and syncs
large state over TCP. It is still not a consensus system: writes are
last-write-wins by version order, compare-and-swap is evaluated against local
state, and there is no quorum. See the *Known limitations* table in
[DESIGN.md](DESIGN.md) for what would have to change before this is useful in
production, and [ROADMAP.md](ROADMAP.md) for the prioritized plan.
