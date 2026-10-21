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

### The corner-case round, with every implementation in one cluster

A third run on the same tablet, this time with all three implementations in
one cluster at once: a Go rendezvous and two Go peers on one machine, the
Electron client as a third node, the tablet as a fourth. What that cast adds
is cross-implementation agreement under conditions a single port cannot
produce.

Held up: 2 MB values, emoji and CJK keys, an empty key, keys containing `/`,
`#`, `?` and `%`, and an empty value — all converged with the Kotlin, Go and
JavaScript folds agreeing byte for byte. TTL expiry, compare-and-swap, a
delete issued while the tablet was off the air (no resurrection on heal), and
conflicting writes on both sides of a Wi-Fi outage all resolved identically on
all four nodes. Two Go peers found each other through the **tablet's**
rendezvous and replicated. The foreground service survived backgrounding, a
swipe from recents and forced deep Doze — a key written while the tablet was
in `deviceidle force-idle` still arrived.

What it found:

* **The node was never restored when the app was relaunched.** `nodeWanted` is
  persisted, but its only reader was `NodeService.onStartCommand`, which cannot
  run after a force-stop or a reboot — precisely the cases where the node is
  gone. Opening the app showed `STOPPED` with *Start node* offered while
  `shared_prefs` still said the user wanted it up; the 2.4 MB store had
  survived, only the node had not. `MainActivity` now starts the service, and
  `RoleRestoreTest` fails the build if it stops consulting the persisted
  intent.
* **A write refused by the store limits was reported as stale** — in this port
  as much as in the other two. See [DESIGN §5.6](../DESIGN.md#56-admission-and-why-a-refusal-is-not-staleness);
  the Diag screen now raises an error for it and the *refused by limits*
  counter sits beside *stale rejected* on the Activity screen.

Two notes for whoever drives this next. The debug APK already on a device may
have been signed with a different debug key, in which case `adb install -r`
fails with `INSTALL_FAILED_UPDATE_INCOMPATIBLE` — back up `shared_prefs/` and
`files/node.json` with `run-as` before uninstalling. And in **Settings**,
tapping a field scrolls the page under the tap, so coordinates captured from
an earlier dump land on the wrong row: re-dump and confirm which field reports
itself focused before typing.

### The chat-sequencing round, on the same tablet

A fourth run, driving the chat sequence over a real radio for the first time:
two Go peers and the desktop client on one machine, the tablet as the fourth
node.

What it confirmed, none of which a simulated network can:

* **A line lost to a genuine Wi-Fi outage is named, asked for and recovered.**
  One line dropped by turning the radio off reported `gaps found 1`, was pulled
  from its author and re-inserted where it was said. An eighty-line outage
  recovered all eighty in order, over four pull rounds — the sixty-four-per-
  request bound doing its job.
* **The sequence survives a force-stop**: 4 → 5 across a restart, with zero
  duplicates, so peers that still remember the old process accept the new one's
  lines instead of discarding them as ones they already hold.
* **Both halves work in both ports.** The tablet asking a Go peer, and a
  headless JavaScript engine making *the tablet* serve a backlog — including
  the "I no longer hold that" answer, which stops the asking.
* `/me` is sequenced; `/msg` deliberately is not and consumes no broadcast
  number.
* **Every line is either delivered or named.** Flooding an author's own
  five-hundred-line ring past a line the tablet still needed left
  468 recovered + 101 reported lost = 569, the author's whole sequence, with
  Diag reading *"101 chat line(s) lost … only while it is still in that node's
  ring. The earliest were from alice."*

What it found, all three fixed in the same round and described in
[DESIGN §5.7](../DESIGN.md#57-chat-delivery-and-the-gap-that-used-to-be-invisible):
a backlog could **introduce** a line nobody asked for, which raised an author's
sequence out of reach and silenced them at that node permanently; a node would
accept a line claiming to be its own, which put it in the log every snapshot is
built from and spread the silence; and a **quiet author's** lines were
unrecoverable, because the baseline waited for them to speak rather than being
set when the two nodes met.

### The split-identity round, with six nodes and a copied state file

A fifth run on the same tablet, with the cast turned up: a Go rendezvous and
four Go peers, the Electron client, and the tablet, all in one cluster.

Held up: pathological keys (empty, 8000 characters, embedded NUL and newline,
`..`, nested `a/b/c`) all written, read back, listed and deleted through the
gateway and replicated identically to every node. A token-protected read-only
gateway refused every mutating route with `403` and every unauthenticated
one — `/health` and `/metrics` included — with `401`. A node whose first seed
was dead joined through the second and synced all 107 keys. Six rapid
*Stop node* / *Start node* toggles with no settling time left one socket, six
peers and no crash. Airplane mode on and off recovered five chat lines through
the repair path and the key written while the tablet was away through
anti-entropy; `am force-stop` followed by a relaunch restored the node by
itself and recovered four more, all nine landing in the persisted log with the
right per-author sequence numbers.

What it found:

* **Two nodes sharing a node id diverge permanently, and the consistency check
  called the cluster converged.** Started two peers from copies of one state
  file and raced a write at both: they landed on the identical version, so
  neither was newer, neither replica ever accepted the other, and 45 seconds of
  anti-entropy left them split — while `/consistency` reported `converged:
  true`, every peer agreeing, and Diag said the node looked healthy. The
  fingerprint folded the version and not the value, so two replicas holding
  different data at one version were indistinguishable to the one tool built to
  find divergence. The fold now covers a hash of the value in all three ports
  (a protocol change — see [DESIGN §14](../DESIGN.md#14-consistency-checking)),
  the report names the key and flags it `same_version`, and the duplicate
  identity itself is reported: each twin names the other, and a third party
  counts how often the id changes hands — 58 flips against 1 for a node that
  genuinely moved address ([DESIGN §14.1](../DESIGN.md#141-two-nodes-under-one-identity)).
* **This port recommended tombstone GC without the warning that makes it
  safe.** Demonstrated on an isolated pair: with a 15 s age, a node stopped
  before a delete came back still holding the value and the delete was undone
  on both nodes, with `/consistency` reporting them converged. That is inherent
  to GC in a last-write-wins store and is documented in
  [DESIGN §5.5](../DESIGN.md#55-tombstone-gc), but the advice here — and in the
  Electron client — said only *"Set one in Settings if deletes are frequent"*,
  dropping the clause the Go port carries about the age having to exceed the
  longest outage. The two ports that expose the setting as a field a non-expert
  can change were the two not warning them.

One note for whoever drives this next. The store fingerprint is a protocol
constant: a device on an older build reports `agrees: false` against every
rebuilt node until its APK is replaced, which looks exactly like real
divergence. Upgrade every node before trusting a report that spans them.

### 2026-10-18 — a tablet, a desktop client and two Go peers on one LAN

PRITOM M10 (Android 16) at 192.168.18.60 with Go `alice` and `bob`, a Go
rendezvous and the Electron client on the host at 192.168.18.79, cluster
`e2elab`. The happy path held in every direction — a write from each of the
three implementations reached the other three and all four folded to the same
fingerprint — and so did the corner cases: a 2 MB value over the stream path,
emoji and CJK keys, a 4 KB key, keys carrying `/ # ? %`, a newline and a tab, a
key of nothing but spaces, an empty value, and a value of invalid UTF-8 refused
with `422`. TTL expired on all three within a second of each other; CAS refused
a stale version and an absent-only write to an existing key. A real Wi-Fi
outage, with writes and chat on both sides, converged on one value with the
tombstone holding; the tablet detected its own gap, pulled the five lines it
had missed and re-inserted them in order. `am force-stop` and a relaunch
brought the node back by itself. The foreground service survived forced deep
Doze. A node started with `-max-keys 5` was correctly reported as refusing
updates rather than as stale, from both sides of the cluster.

What it found:

* **Every piece of typed state was lost on rotation.** An open key dialog was
  dismissed outright, taking the key, value and TTL with it; the chat composer
  emptied mid-sentence; unsaved **Settings** edits reverted. The navigation
  state was already `rememberSaveable` and only the forms were not, so the app
  came back on the right screen with the work gone, which reads as having
  discarded it on purpose. A tablet is the device that gets rotated. All the
  form state is saveable now, and `repo.test.js` fails on a screen that holds
  typed text in state a rotation discards.
* **A refused setting was kept on screen with nothing in effect behind it.**
  Typing `0` into *Node port* and pressing *Apply* correctly stored 3137 — and
  left the field reading `0`, with no message. The form only re-seeds when the
  stored settings change, so editing *just* the bad field is exactly the case
  with no feedback at all. The refused fields now snap back to what is in
  effect and the screen says which value was not kept; the desktop client
  raises the same thing as a warning, where it had been toasting "Settings
  applied" over a silent revert.
* **A state file that could not be read was lost in silence.** The node starts
  anyway — losing the store is bad and refusing to start is worse — but it
  starts with an empty replica *and a freshly minted identity*, and then
  rewrites the unreadable file with that new state. This port caught the
  exception and returned null, so nothing said any of it had happened.
  Confirmed on the tablet by corrupting `files/node.json`: the backup held node
  id `45ae329a…`, the rewritten file `36c5c470…`. **Diag** now raises an error
  naming the file, the parse failure and both consequences.
* **This port's rendezvous never reported the address it observed.** See
  [DESIGN §13](../DESIGN.md#13-diagnostics): the roster carries `SeenAt`, which
  is the only way a node bound to a wildcard can learn which of its addresses
  the cluster reaches it on. Only the Go service sent it and only the Go node
  read it, so a peer joining through this app fell back to the loopback guess,
  and a Go node run with `-advertise auto` against it resolved to `0.0.0.0` and
  published that to the whole cluster. Both halves are implemented here and in
  the desktop client now, verified on this LAN in both directions.

  Fixing it caused one regression that only the radio could show: with Wi-Fi
  off a device advertises loopback because it has nowhere else to advertise,
  and the address the rendezvous saw belongs to the network it has just left.
  Reporting it then replaced *"Connect to a network"* with *"set the advertise
  host in Settings to 192.168.18.60"* — wrong then, and still wrong on the next
  network joined. The rule defers to the `offline` flag now, which is what that
  flag is for.

### 2026-10-21 — the diagnostics round, four nodes on one LAN

PRITOM M10 (Android 16, 1280x800) at 192.168.88.31 with Go `alice` and `bob`, a
Go rendezvous and the Electron client on the host at 192.168.88.65, cluster
`hwlan`. The happy path held in every direction again — a write from each of
the three implementations reached the other three, chat and `/me` crossed all
four, and `/consistency` folded to the same answer from every side, including a
200 KB value over the stream path. So did the corner cases: a guarded write
refused by name from the Android dialog, `If-Match` claiming a lock on one node
and being refused `412` on another, a TTL key expiring within a second
everywhere, a value of invalid UTF-8 refused `422` while newlines, tabs, control
characters and emoji crossed byte for byte, *Stop node* dropping the tablet from
the Go roster at once, identity and store surviving `am force-stop`, the
foreground service holding the node in the cluster through a 60 s screen-off,
and rotation keeping an open key dialog's typed text. A wrong pre-shared key
isolated the tablet and Diag named the seed and the fingerprint rather than the
network.

What it found — all three in the diagnostics, all three fixed in every
implementation, and all three with the hardware symptom written into the test
that now guards them:

* **A healthy node reported its own rendezvous service as an unknown source.**
  The rule behind *"N address(es) sent packets without being a peer"* asks
  whether a packet came from an address in the peer table; a rendezvous answers
  with a roster and is by design never a peer, so it matched — permanently, and
  in the words of a NAT misconfiguration. It only shows when the roster comes
  back by datagram, the fallback taken whenever the stream exchange cannot be
  made, which on this tablet was every join: the Go and desktop nodes beside it
  called the same cluster healthy while it warned. A roster now marks the
  address it arrived from, which also covers a seed written as a hostname.
* **One failed send warned for the life of the node.** The counter only climbs,
  and on a phone it always climbs: this tablet's Wi-Fi drops about 3% of plain
  ICMP, so `sendto` returns `ENETUNREACH` every few minutes while gossip,
  replication and chat all work. A warning that cannot clear is one the reader
  learns to scroll past. The last failure carries a timestamp now and the
  finding drops to informational after two quiet minutes; the total stays on the
  traffic counters.
* **A peer that left never stopped being an anomaly.** One packet from an
  address evicted a moment earlier put it on the unknown-source list for good —
  seen here when the tablet was moved to a wrong key and its former cluster went
  on reporting it. An address that has been quiet for five minutes now drops off
  the list, while a genuinely misconfigured node, which keeps talking, stays on
  it.

Two things were checked and are *not* bugs, which is worth recording so the next
run does not chase them: the send failures are the radio, not the app — plain
`ping` from the device loses the same 3% — and a peer showing *"never gossiped"*
shortly after a restart is only a gossip tick that has not come round yet, since
each node gossips to one random peer per interval.

## Known gaps

* The battery cost of the 10 s gossip tick is still unmeasured; **Diag**
  reports whether Doze applies and links to the exemption setting.
* No import/export UI yet; the engine supports both.
* **Settings** does not expose the store limits (`maxValueBytes`, `maxKeys`).
  They are `NodeConfig` options and enforced by the engine against a peer as
  well as a local writer, but only the Go node has flags for them.
* Editing **Settings** and navigating away before *Apply* discards the edit,
  with nothing on screen to say so. A rotation no longer does this — the fields
  are `rememberSaveable` — but leaving the screen still re-seeds them from
  storage.
