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
* **Activity** — replication as it happens: what was applied, what was
  rejected as stale, what anti-entropy pushed or pulled — alongside the
  counters. All of this is otherwise invisible, which is when a replication
  bug hides.
* **Bootstrap** — run the rendezvous service here, so the phone is the
  meeting point a LAN cluster forms around. It never sees KV data or chat.
* **Settings** — nickname, port, advertised host, bootstrap seeds, cluster
  name, pre-shared key, and the tombstone GC age.

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
│   ├── Metrics.kt        counters
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
* `GoInteropTest` joins a **running** Go cluster and replicates through it in
  both directions. It skips unless pointed at one:

```
REZOAGWE_GO_BOOTSTRAP=127.0.0.1:19999 REZOAGWE_GO_HTTP=127.0.0.1:18081 \
REZOAGWE_GO_PSK=demo REZOAGWE_GO_CLUSTER=e2e \
  ./gradlew :app:testDebugUnitTest --tests '*GoInteropTest'
```

## Known gaps

* Verified against a live Go cluster from the JVM, but not yet run on a
  physical phone — Doze behaviour and the battery cost of the 10 s gossip
  tick are unmeasured.
* No import/export UI yet; the engine supports both.
