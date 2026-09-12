# Rezoagwe — Roadmap

Prioritized backlog. Items are numbered by **global priority** (1 = do next)
and grouped into thematic tiers. Effort is a rough T-shirt size: **S** ≈ a few
hours, **M** ≈ about a day, **L** ≈ multi-day.

This complements the *Known limitations & next steps* table in
[DESIGN.md](DESIGN.md#11-known-limitations--next-steps); each open item notes
the code it touches.

## Shipped

The repository is at **0.2.0** — the version the Makefile stamps into the Go
binaries and their `.deb`, the desktop client's `package.json`, and the Android
app's `versionName`; a test keeps the four in step. 0.2.0 adds the desktop
client and its packaging to the 0.1.0 release.

Everything the previous backlog listed is done. Newest first:

- ✅ **Android on hardware** — run on a PRITOM M10 tablet against a Go cluster
  on a real LAN. It closed the item and found the bug behind it: every UDP send
  made from a Compose callback was silently dropped, so a UI write reached peers
  only on the next anti-entropy round and *Stop node* never sent its `Goodbye`.
  Dispatching to `Dispatchers.IO` took that write from ~10 s to 234 ms, and
  `UiThreadingTest` now fails the build if the pattern comes back. Battery cost
  of the 10 s tick is still unmeasured.
- ✅ **Hermetic end-to-end suite** (`make e2e`) — a rendezvous, three peers, a
  late joiner, a leaver, a restarting node and two strangers in containers on a
  private network, driven through the HTTP gateway. It tests what only exists
  between processes: real sockets, real framing, real timing.

- ✅ **Desktop app** — a JavaScript port of both roles (node + rendezvous
  service) in Electron, with the cluster drawn as a graph, a diagnostics screen
  that turns counters into causes, and a split view. Verified against a live Go
  cluster; its own suite runs a whole cluster in one process over an in-memory
  network, so convergence under packet loss and a partition are asserted rather
  than hoped for.

- ✅ **Android app** — Kotlin port of both roles (node + rendezvous service)
  with a Compose UI, a foreground service, and parity tests against the Go
  codec. Verified live against a running Go cluster. Adapts to tablets
  (navigation rail, two panes), draws the cluster as a graph derived from
  gossiped peer lists, and reports its own health on a diagnostics screen —
  which is what surfaced the `null`-slice decode gap below.
- ✅ **Go `null` slices decoded** — Go marshals a nil slice as `null` for
  fields without `omitempty`, so an empty node's digest (`entries: null`)
  authenticated and then failed to parse in the Kotlin app, dropping the
  anti-entropy round it carried. The decoder now coerces those to empty.
- ✅ **Wire v2** — one authenticated framing for every packet, node-to-node
  and node-to-bootstrap; protobuf removed entirely.
- ✅ **Pre-shared key + HMAC + replay guard** — every packet carries a MAC
  over a cluster-derived key, with a nonce and timestamp; foreign packets
  are dropped before any handler sees them.
- ✅ **Anti-entropy reconciliation** — range-bounded per-key digests on the
  gossip tick, with push and pull repair. A dropped write is now re-sent.
- ✅ **Chunked state sync over TCP** — streams carry length-prefixed frames,
  so a store or roster larger than a datagram syncs instead of failing.
- ✅ **Compare-and-swap** — guarded writes and deletes, exposed in the TUI,
  the HTTP gateway (`If-Match`) and the Android app.
- ✅ **Key TTL / expiry** — deterministic, version-preserving expiry that
  needs no replication of its own.
- ✅ **Tombstone GC** — age-based, opt-in via `-tombstone-ttl`.
- ✅ **HTTP/REST gateway** — `-http` with CRUD, CAS, TTL, history, peers,
  chat, export/import, health and Prometheus metrics.
- ✅ **Metrics** — counters for traffic, rejected packets, replication and
  anti-entropy, in the TUI and as Prometheus exposition.
- ✅ **KV activity feed** — remote applies, stale rejections and repair
  traffic are visible instead of silent.
- ✅ **Key history** — per-key version ring showing who wrote what.
- ✅ **Import / export** — versioned interchange, merge or seed.
- ✅ **Transport abstraction + lossy in-memory network** — multi-node
  convergence is asserted under packet loss, not hoped for.
- ✅ **Persistent node identity** — a node keeps its version tiebreak across
  restarts and address changes.
- ✅ **Chat enhancements** — `/nick`, `/me`, direct messages, and a command
  vocabulary shared by every front end; chat persisted to disk.
- ✅ **Multiple bootstrap seeds** — `-bootstrap a,b,c`.
- ✅ **Bootstrap hardening** — persisted roster, stream + datagram service,
  authenticated registration, nicknames in the roster, first tests.
- ✅ **Real diagnostics** — `-debug` with `-logfile`, never scribbling on the
  TUI.
- ✅ **Headless mode** — `-headless` on both binaries.
- ✅ **Version via ldflags** — the binary reports what the Makefile built.
- ✅ **Socket reuse and peer-address validation** — one socket per node;
  malformed gossip is refused at the door.

Earlier milestones: versioned last-write-wins, robust bootstrap join,
chat-history sync on join, local persistence, graceful leave.

---

## Tier 1 — Correctness

### 1. Merkle-tree digests — **M**
Replace the flat per-key digest with a tree over key ranges, so
reconciliation costs `O(log n)` rounds instead of walking the keyspace.
**Why:** the current cursor covers a large store batch by batch; a cluster
with a million keys converges slowly after a long partition. **Notes:**
`KVStore.Digest`/`Reconcile` already isolate the comparison, so this swaps
the summary type without touching the engine.

### 2. Quorum state sync — **S–M**
Pull from several peers on join and merge all responses.
**Why:** a joiner currently trusts one random peer, so it inherits that
peer's gaps until anti-entropy fills them in.

### 3. Acked membership — **M**
Ping/ack with a suspicion phase (SWIM-style) instead of pure last-seen
eviction. **Why:** a busy-but-silent peer looks dead, and a dead peer's
eviction is only as timely as the eviction tick.

## Tier 2 — Security

### 4. Encrypted payloads — **M**
The frame is authenticated but the body is plaintext. Add AEAD (the nonce
is already there) so a KV value is not readable on the wire.

### 5. Per-node keys — **L**
One shared psk means any member can impersonate any other. Per-node
keypairs with signed Hellos would close that.

## Tier 3 — Reach

### 6. mDNS / DNS-SD discovery — **S–M**
Find peers on a LAN with no seed configured at all — the setup step most
likely to defeat a new user, and the one the Android app feels most.

### 7. Watch / subscribe — **M**
Long-poll or SSE on `/kv?watch=`, plus a callback in the engine.
**Why:** every consumer currently polls; the engine already knows exactly
when a key changed.

### 8. Range and prefix queries on the wire — **S**
`/kv?prefix=` filters locally after listing everything. A prefix-scoped
state request would make a large store usable from a small client.

### 9. Desktop background mode — **S**
Closing the window stops the node on Linux and Windows, because there is no tray
icon to keep the app alive. **Why:** the same objection the Android foreground
service answers — a node that only runs while its window is open is evicted by
its peers seconds after it is minimised away. **Notes:** `electron/src/main.js`
already routes quitting through a `Goodbye`, so this is a tray icon and a
"close to tray" setting, not a lifecycle change.

## Tier 4 — Operability

### 10. CI — **S**
Nothing runs the tests automatically, and there are now four suites to run:
`go test -race ./...`, the Android unit tests, the desktop client's
(`make electron-test`, which needs no display) and `make e2e`. The first three
belong on every push; the containerised one is slow enough to earn its own job.

### 11. Structured logging — **S**
`-logfile` writes logrus text. JSON lines with the node id would make a
multi-node run greppable.

### 12. Backpressure on repair traffic — **S**
`maxPush`/`maxPull` bound one round, but nothing rate-limits successive
rounds; a node that falls far behind gets a burst per gossip tick.
