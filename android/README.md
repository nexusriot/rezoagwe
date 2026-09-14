# R3zo Agwe for Android

The rezoagwe node **and** the rendezvous service on a phone, speaking the
same wire protocol as the Go implementation.

## What it does

* **Keys** — the KV store: create, edit and delete, with an optional TTL and
  a *Guard* checkbox that turns the save into a compare-and-swap against the
  version that was on screen. Each row shows its version and, for a key with
  an expiry, the time it has left. Per-key version history is one tap away.
* **Chat** — messages, `/me` emotes, direct messages, and the same slash
  commands the terminal client has (`/help` lists them).
* **Peers** — who is in the cluster, with nicknames and last-seen ages, plus
  the start/stop control for the node itself.
* **Graph** — the cluster drawn as a graph: this node in the middle, its
  peers on the first ring, and nodes it only knows from gossip further out.
  Links both ends confirm are solid, links only one end claims are dashed.
  A peer list cannot show that two of your peers do not talk to each other,
  or that the cluster has split in two; both are visible here. Pinch to
  zoom, drag to pan, tap a node for its details — and, for a peer, to pull
  its state or drop the link on purpose and watch the cluster heal.
* **Activity** — replication as it happens: what was applied, what was
  rejected as stale, what anti-entropy pushed or pulled — alongside the
  counters. All of this is otherwise invisible, which is when a replication
  bug hides.
* **Diag** — the whole node in one screen, starting with what is wrong.
  Health checks turn counters into causes ("packets failed authentication"
  → the key or cluster name differs, with the key fingerprint to compare);
  then identity, sockets, the device's own networking, per-peer traffic,
  addresses that send packets without being peers, store shape and the
  topology. *Copy report* puts all of it on the clipboard for a bug report.
* **Bootstrap** — run the rendezvous service here, so the phone is the
  meeting point a LAN cluster forms around. It never sees KV data or chat.
* **Settings** — nickname, port, advertised host, bootstrap seeds, cluster
  name, pre-shared key, and the tombstone GC age.

## Phones and tablets

The layout follows the window, not the device:

* under 600dp wide — tabs across the top, one screen at a time
* 600dp and up, or a short landscape window — a navigation rail beside the
  content, which buys back the height the tab strip costs
* 880dp and up (and tall enough) — two screens side by side, each captioned,
  with the pairing chosen so the second answers what the first raises: peers
  next to the graph, chat next to peers, settings next to diagnostics. The
  split can be collapsed from the header.

Every screen pads for the status bar, the navigation bar, a display cutout
and the keyboard, so nothing renders under the clock and the chat input rises
with the keyboard instead of hiding behind it.

A foreground service keeps whichever roles are running alive: a gossip node
that only runs while its screen is open is not participating in a cluster,
since peers evict it seconds after the phone sleeps.

## Build

```
./gradlew :app:assembleDebug     # → app/build/outputs/apk/debug/app-debug.apk
./gradlew :app:testDebugUnitTest # unit tests
```

`local.properties` points at the Android SDK. From the repository root,
`make android` builds the debug APK into `dist/`.

## Talking to a Go cluster

Point the app at a bootstrap seed reachable from the phone, and give it the
same cluster name and pre-shared key:

```
rezoagwe-bootstrap -headless -port 9999 -psk demo -cluster home
rezoagwe-discovery -headless -node 192.168.1.10:3137 -bootstrap 192.168.1.10:9999 \
    -psk demo -cluster home -http 127.0.0.1:8080
```

Then in **Settings**: seeds `192.168.1.10:9999`, cluster `home`, key `demo`.
Leave *Advertise host* blank and the app uses its own LAN address, which is
what peers need to reach it.

Nodes only talk to peers with the same cluster **and** key. Without a key the
framing key comes from the cluster name alone: that separates two clusters
sharing a network, but provides no secrecy.

## Layout

```
app/src/main/java/com/nexusriot/rezoagwe/
├── proto/          frame codec (HMAC + replay guard) and message bodies
├── net/            datagram + stream transport, length-prefixed framing
├── core/
│   ├── KvStore.kt        versioned store: LWW, CAS, TTL, digests, GC
│   ├── NodeEngine.kt     membership, replication, chat, anti-entropy
│   ├── BootstrapServer.kt the rendezvous role
│   ├── Topology.kt       the cluster graph, derived from gossiped peer lists
│   ├── Diagnostics.kt    one snapshot of the node, plus the health checks
│   ├── DeviceInfo.kt     the phone's own networking and power state
│   ├── Metrics.kt        counters, per-peer traffic and rates
│   ├── Persistence.kt    atomic, generation-guarded state file
│   └── Runtime.kt        process-wide holder for both roles + settings
├── service/        foreground service
└── ui/             Compose screens
```

## Parity with the Go implementation

The app shares no code with the Go node — only the protocol — so the rules
that matter (framing, last-write-wins, digest ranges, version-preserving
expiry) are duplicated deliberately and pinned by tests:

* `CodecTest` decodes a frame produced by the **Go** codec and checks the
  derived keys against fixed vectors from `proto.DeriveKey`.
* `KvStoreTest` asserts the same merge, CAS, TTL and reconciliation rules the
  Go store's tests assert.
* `WireCompatTest` decodes the bodies Go actually emits. Go marshals a nil
  slice as `null` for any field without `omitempty`, so an empty Go node's
  digest arrives as `{"from":"…","entries":null}` — which used to
  authenticate and then fail to parse, silently dropping the repair it
  carried. The decoder now coerces those nulls to empty.
* `GoInteropTest` joins a **running** Go cluster and replicates through it in
  both directions. It skips unless pointed at one:

```
REZOAGWE_GO_BOOTSTRAP=127.0.0.1:19999 REZOAGWE_GO_HTTP=127.0.0.1:18081 \
REZOAGWE_GO_PSK=demo REZOAGWE_GO_CLUSTER=e2e \
  ./gradlew :app:testDebugUnitTest --tests '*GoInteropTest'
```

* `ParityTest` holds this port to the correctness rules the Go node's tests
  assert — the anti-entropy digest never wider than one repair round, a
  keyspace cursor per peer, store limits enforced against a peer as well as a
  local writer — and pins `KvStore.fingerprint()` to the **same fixed vectors**
  the Go and desktop suites use. A fold that differed between ports would
  report two converged replicas as divergent, which is the loudest possible
  false alarm from the one feature whose job is to be believed.

## Verified on hardware

Run on a PRITOM M10 tablet (Android 16, 1280x800) against a Go cluster on the
same Wi-Fi LAN: all three layout tiers, inset and keyboard handling, Keys CRUD
with TTL and a refused *Guard* write, chat both ways including `/me` and `/msg`,
the Graph's node detail / *Sync from* / *Forget* and the heal that follows,
Diag's health checks, the phone as the rendezvous service accepting a real Go
node, and the foreground service surviving screen-off, the loss of adb and a
full restart of the Go cluster.

That run is also what found the bug this app's tests now guard against: **every
UDP send made from a Compose callback was silently dropped**, because Android
refuses a socket write on the main thread and the failure was being counted
rather than surfaced. A UI key write took ~10 s to reach a peer (it arrived only
on the next anti-entropy round) and *Stop node* never sent its `Goodbye` at all.
Dispatching those calls to `Dispatchers.IO` took the same write to 234 ms and the
goodbye to 285 ms. `UiThreadingTest` scans `ui/` and `service/` and fails on any
call in its denylist that is not inside a `Dispatchers.IO` block, so the
regression cannot come back quietly.

### The consistency-check round, on the same tablet

A second run, after this port gained the store fingerprint and the *Verify
replicas* action, against a three-node cluster on a real Wi-Fi LAN: a Go
rendezvous and two Go peers on one machine (`-advertise` giving them routable
addresses), the tablet joining as the third.

What it proved that no unit test can:

* **A Kotlin fold and a Go fold agree byte for byte across a network.**
  `GET /consistency` on a Go peer reported the tablet `reachable: true,
  agrees: true` — before this round it could only have reported it unreachable.
* **Divergence is detected *and* localised, then heals.** Forty keys written to
  the Go node at once left the tablet holding 14 of 45; the check named it
  immediately — `agrees: false`, **15 of 16 buckets differing** — and eight
  seconds later, one anti-entropy round on, the same check reported the whole
  cluster converged. That is the feature's entire argument, observed rather
  than asserted.
* **The check runs in both directions.** *Verify replicas* on the Diagnostics
  screen dialled both Go peers over TCP and reported `agrees — 45 keys` for
  each. It dispatches to `Dispatchers.IO`, so `UiThreadingTest` covers it.
* The per-peer sweep line renders (`all 2 peer(s) at the start of the
  keyspace`, correct for a 45-key store against a 128-key digest), a UI key
  write reached both Go nodes including the one it was never sent to, chat
  flowed both ways, and *Stop node* dropped the tablet from the Go roster in
  3.4 s via its `Goodbye` rather than the 15 s eviction timeout.

Two things worth knowing for the next run. Settings edits are **discarded if you
navigate away before pressing Apply** — the fields are `remember(saved)`, so a
tab switch recomposes them from what is stored. And the stock IME appends a
space after a word-like token, so `e2edev` was typed and `e2edev ` was in the
field; `Settings.sanitized()` trimmed it on Apply, which is the 2026-09-06 fix
still doing its job. The diagnostics also caught five transient
`ENETUNREACH` send failures and named the peer and cause — Wi-Fi, not the app.

## Known gaps

* The battery cost of the 10 s gossip tick is still unmeasured; **Diag**
  reports whether Doze applies and links to the exemption setting.
* No import/export UI yet; the engine supports both.
* **Settings** does not expose the store limits (`maxValueBytes`, `maxKeys`).
  They are `NodeConfig` options and enforced by the engine against a peer as
  well as a local writer, but only the Go node has flags for them.
* Editing **Settings** and navigating away before *Apply* discards the edit,
  with nothing on screen to say so.
