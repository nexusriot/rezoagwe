## R3zo Agwe

_Distributed key-value store with an embedded chat, written in Go —
with an Android app that speaks the same protocol._

Two cooperating binaries:

* **`rezoagwe-bootstrap`** — rendezvous service. Knows only the *set of
  node addresses*; never sees KV data or chat.
* **`rezoagwe-discovery`** — full peer node. Owns a local KV replica,
  participates in chat, gossips peer membership, reconciles divergence
  with anti-entropy, and runs a `tview` TUI (or none, with `-headless`).

A new node contacts bootstrap **once at startup** to learn the initial
roster. From then on, peers gossip among themselves.

For internals, wire protocol, and the failure model, see
[DESIGN.md](DESIGN.md).

### **Why?**

![Pic](https://github.com/nexusriot/rezoagwe/blob/main/wtf.png)

### **Design (concept)**

![Pic](https://github.com/nexusriot/rezoagwe/blob/main/rezo_agwe.png)
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
  and chat are written atomically after every change and reloaded on
  startup (`-data`, default `<config dir>/rezoagwe/<node>.json`; `-data -`
  disables). Node identity survives a restart, so a node keeps its place in
  the version ordering even if it moves to another port
- **HTTP gateway** (`-http`): `GET/PUT/DELETE /kv/{key}` with `If-Match`
  compare-and-swap and TTL headers, plus history, peers, chat, activity,
  export/import, health and Prometheus metrics
- **Replication metrics and an activity feed**: remote applies, stale
  rejections, refused guarded writes and repair traffic are visible rather
  than silent
- **Key history**: the recorded versions of a key, and who wrote each
- Robust join: an unreachable or lossy bootstrap can't stall startup, and
  the node keeps retrying while it knows no peers; several seeds can be
  given as `-bootstrap a,b,c`
- Peer membership by periodic gossip + Hello replies, graceful `Goodbye` on
  quit, and stale-peer eviction
- Built-in chat with nicknames, `/me` emotes, direct messages, per-sender
  colours and slash commands shared by every front end
- Per-node TUI on `tview`/`tcell`: keys list with filter and expiry
  markers, details, peer list, a feed pane that toggles between chat and
  replication activity, modals for history, metrics, help and
  import/export, and a status bar
- **Android app** (`android/`): the same node *and* the rendezvous service
  on a phone, with a Compose UI and a foreground service
- Cross-build to Linux (amd64/i386/arm64/armv7/riscv64), FreeBSD, macOS,
  Windows; Debian packages

---

### Build

```
make x86_64           # linux/amd64, dynamic
make x86_64-static    # linux/amd64, fully static (CGO off)
make all              # every cross-build target into dist/
make debs             # deb-amd64 + deb-i386 + deb-arm64 + deb-armhf
make android          # debug APK into dist/
make test-race        # the full test suite under the race detector
make help             # full list of targets
```

A single `.deb` installs both `/usr/bin/rezoagwe-bootstrap` and
`/usr/bin/rezoagwe-discovery`. To package a single arch directly:

```
./build-deb.sh amd64
./build-deb.sh i386
./build-deb.sh arm64    # or: ./build-deb-arm64.sh
./build-deb.sh armhf
```

### Usage

```
./rezoagwe-bootstrap [-port number] [-psk key] [-cluster name] [-headless]
./rezoagwe-bootstrap -port 9999
```
default port is **9999** (UDP and TCP)

```
./rezoagwe-discovery [-bootstrap seeds] [-node addr] [-nick name] [-data file]
                     [-psk key] [-cluster name] [-http addr] [-headless]
./rezoagwe-discovery -bootstrap :9999 -node :3137 -nick alice
./rezoagwe-discovery -bootstrap :9999 -node :3138 -nick bob
```

defaults: `-bootstrap :9999`, `-node :3137`, `-nick anon`,
`-cluster rezoagwe`. The second node pulls alice's store on join, both see
each other in the **Nodes** pane, chat flows both ways, and KV writes
propagate.

Nodes only talk to peers with the same `-cluster` **and** `-psk`. Without a
`-psk` the framing key comes from the cluster name alone: that separates two
clusters sharing a network, but provides no secrecy.

### Hotkeys (discovery TUI)

| Key             | Action                                          |
|-----------------|-------------------------------------------------|
| `c`             | Create key (value, TTL, optional guard)         |
| `e` / `Enter`   | Edit key under cursor                           |
| `d`             | Delete key under cursor                         |
| `h`             | Version history of the key under cursor         |
| `/`             | Focus the filter; type to filter keys/values    |
| `a`             | Switch the feed between chat and activity       |
| `m`             | Replication metrics                             |
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
```

See [DESIGN.md §8](DESIGN.md#8-http-gateway) for the whole surface.

### Android

`android/` builds an app that runs the node, the rendezvous service, or
both — with the KV store, peers, chat, activity feed and metrics on screen,
and a foreground service so the node keeps gossiping when the screen is
off.

```
cd android && ./gradlew :app:assembleDebug
```

It shares no code with the Go implementation, only the protocol, so parity
is pinned by tests: a frame produced by the Go codec is decoded by the
Kotlin one, and an opt-in live test joins a running Go cluster and
replicates through it in both directions.

### Status

Replication converges concurrent writes via per-key last-write-wins, repairs
dropped packets via anti-entropy, authenticates every packet, and syncs
large state over TCP. It is still not a consensus system: writes are
last-write-wins by version order, compare-and-swap is evaluated against local
state, and there is no quorum. See the *Known limitations* table in
[DESIGN.md](DESIGN.md) for what would have to change before this is useful in
production, and [ROADMAP.md](ROADMAP.md) for the prioritized plan.
