# Rezoagwe — a step-by-step manual

Everything here was run against **0.4.8** on a real LAN: two Go nodes and a Go
rendezvous on a laptop, the Electron desktop client beside them, and the Android
app on a tablet, all in one cluster. Where a step has a trap in it, the trap is
written down next to the step rather than left for you to find.

For *why* any of it works the way it does, see [DESIGN.md](DESIGN.md). This file
is only about using it.

---

## 0. The one-minute version

A rezoagwe cluster is a set of **nodes** that each hold a full copy of the same
key-value store and the same chat log. There is no leader and no server: every
node is a replica, and they reconcile continuously. One extra process, the
**rendezvous service**, exists only so nodes can find each other on first
contact — it never sees the data.

Three things have to match on every node or they will not talk at all:

| Must match | Why |
|---|---|
| **cluster name** | feeds the key every packet is authenticated with |
| **pre-shared key** | same |
| the network | they have to be able to reach each other |

If two nodes disagree on either of the first two, they are simply deaf to each
other — no error, no half-join. Every screen shows a **key fingerprint**; if two
nodes show different fingerprints, that is the problem, and nothing else is.

---

## 1. Build what you need

From the repository root:

```bash
make x86_64
```

That produces three binaries in `dist/`, each stamped with the version and the
platform: `rezoagwe-bootstrap-<version>-linux-amd64` (the rendezvous service),
`rezoagwe-discovery-<version>-linux-amd64` (a node) and
`rezoagwectl-<version>-linux-amd64` (a command-line client). `make all`
cross-builds every supported target instead.

The commands below use short names for readability. Either symlink them once —

```bash
V=$(sed -n 's/^VERSION *?= *//p' Makefile)
for b in rezoagwe-bootstrap rezoagwe-discovery rezoagwectl; do
  ln -sf "$b-$V-linux-amd64" "dist/$b"
done
```

— or run `go run ./cmd/discovery` and friends straight from the source tree.

The Android app:

```bash
make android
```

→ `dist/rezoagwe-<version>-android-debug.apk`.

The desktop client:

```bash
make electron
```

Packaged builds — `.deb`, AppImage, cross-compiled binaries — are in
[README.md](README.md); they are not needed to follow this manual.

---

## 2. Start a cluster on one machine

Three terminals. Pick a cluster name and a key and use the same ones everywhere.

**Terminal 1 — the rendezvous service.**

```bash
./dist/rezoagwe-bootstrap -port 9999 -cluster demo -psk s3cret
```

**Terminal 2 — the first node.**

```bash
./dist/rezoagwe-discovery -node :3137 -bootstrap 127.0.0.1:9999 -cluster demo -psk s3cret -nick alice
```

**Terminal 3 — a second node.**

```bash
./dist/rezoagwe-discovery -node :3138 -bootstrap 127.0.0.1:9999 -cluster demo -psk s3cret -nick bob
```

Both nodes open a terminal UI. Within about five seconds each should list the
other under **Nodes**. Press `c` in either one, type a key and a value, and
watch it appear in the other.

> **Each node needs its own port.** Two nodes on one machine cannot share 3137.
> If a port is taken the node says so and stops; nothing silently half-starts.

> **Each node also needs its own state file.** The default path is derived from
> the bind address, so two nodes on different ports already get different files.
> If you pass `-data` explicitly, give each node a different path — two nodes
> started from *copies of one state file* share an identity, and that is the one
> way to make a cluster diverge permanently. They will each report the other
> under **Diag**, but prevention is easier than the cure.

---

## 3. Across a real network

Exactly the same, except every node has to advertise an address its peers can
actually dial. A node bound to a wildcard (`:3137`, `0.0.0.0:3137`) knows every
address its machine has and not which one its packets leave by, so by default it
advertises the wildcard — which no peer can reach.

Three ways to get it right, best first:

```bash
# 1. Ask the rendezvous service where it sees you from. It is the only
#    component that can see this, and all three implementations now report it.
./dist/rezoagwe-discovery -node :3137 -advertise auto \
    -bootstrap 192.168.1.10:9999 -cluster demo -psk s3cret

# 2. Say it yourself.
./dist/rezoagwe-discovery -node :3137 -advertise 192.168.1.23:3137 \
    -bootstrap 192.168.1.10:9999 -cluster demo -psk s3cret

# 3. Bind the specific address, which then is what gets advertised.
./dist/rezoagwe-discovery -node 192.168.1.23:3137 \
    -bootstrap 192.168.1.10:9999 -cluster demo -psk s3cret
```

If you get it wrong, you do not have to guess: **Diag** (`D` in the TUI,
`-doctor`, or `GET /diagnostics`) says

> Advertising an address the cluster does not see this node at — Peers are told
> to reach this node at 0.0.0.0:3137, and the rendezvous service received its
> packets from 192.168.1.23:48812. … Pass `-advertise 192.168.1.23:3137`.

It names the value to use, because the rendezvous saw it.

> **On a LAN with no rendezvous at all**, `-mdns` finds peers over multicast
> DNS. It does not work across subnets and is commonly blocked on guest Wi-Fi;
> a `-bootstrap` seed works where multicast does not.

---

## 4. The terminal UI

| Key | Action |
|---|---|
| `c` | Create a key (value, TTL, optional guard) |
| `e` / `Enter` | Edit the key under the cursor |
| `d` | Delete the key under the cursor |
| `h` | Version history of the key under the cursor |
| `/` | Focus the filter; type to filter keys and values |
| `Enter` in the filter | Back to the list, filter kept |
| `Esc` in the filter | Clear the filter and go back |
| `a` | Switch the feed between chat and activity |
| `m` | Replication metrics |
| `g` | Cluster graph: peers, links, partitions |
| `D` | Diagnostics — what is actually wrong |
| `v` | Verify every replica holds the same store |
| `x` / `i` | Export / import the store |
| `?` | Help |
| `Tab` | Cycle focus: Keys → Nodes → Feed → Message |
| `Enter` in chat | Send |
| `Ctrl+Q` | Quit, announcing departure to peers |

**Guard** in the edit form makes the save a compare-and-swap against the version
that was on screen: if a peer changed the key meanwhile, the save is refused
rather than silently overwriting their write.

Chat accepts commands:

```
/nick <name>   /me <text>   /msg <peer> <text>   /peers   /keys
/get <key>     /set <key> <value>   /setttl <key> <seconds> <value>   /del <key>
```

`/msg` is a direct message: it goes to one peer only, and deliberately does not
consume a broadcast sequence number.

---

## 5. Headless, and the HTTP gateway

A node with no terminal, driven over HTTP:

```bash
./dist/rezoagwe-discovery -headless -node :3137 -http 127.0.0.1:8137 \
    -bootstrap 127.0.0.1:9999 -cluster demo -psk s3cret -nick api
```

| Route | Method | What it does |
|---|---|---|
| `/health` | GET | this node's own status |
| `/metrics` | GET | Prometheus exposition text |
| `/peers` | GET | who this node can see |
| `/activity` | GET | the activity feed |
| `/kv` | GET | list keys (`?prefix=`, `?watch=1`) |
| `/kv/{key}` | GET PUT DELETE | read, write, delete one key |
| `/history/{key}` | GET | the recorded versions of a key |
| `/chat` | GET POST | read the conversation, say a line |
| `/export` `/import` | GET POST | the whole store out and back |
| `/snapshot` | POST | write a snapshot now |
| `/repair` | POST | make this node's copy of named keys win |
| `/topology` | GET | the cluster as a graph |
| `/diagnostics` | GET | what is wrong, in JSON or `?format=text` |
| `/consistency` | GET | ask every replica whether they agree |

```bash
curl -X PUT  localhost:8137/kv/greeting -d 'hello'
curl         localhost:8137/kv/greeting
curl -X DELETE localhost:8137/kv/greeting
```

### Expiry and conditional writes

TTL is a **header**, not a query parameter:

```bash
curl -X PUT localhost:8137/kv/session -H 'X-Rezoagwe-TTL: 300' -d 'token'
```

> `?ttl=300` is silently ignored, as any unknown query parameter is. You get a
> `204` and a key that never expires. Use the header.

Compare-and-swap uses `If-Match` with the ETag a `GET` returns:

```bash
# tr -d '\r' first: HTTP header lines end CRLF, and a $-anchored sed pattern
# will not match with the CR still on the end. Get that wrong and ETAG is
# empty — which, by the curl trap below, turns the next line into an
# unconditional write that happily returns 204.
ETAG=$(curl -sD- -o/dev/null localhost:8137/kv/counter | tr -d '\r' | sed -n 's/^Etag: "\(.*\)"$/\1/p')
curl -X PUT localhost:8137/kv/counter -H "If-Match: $ETAG" -d '2'   # 204, or 412 if a peer got there first
```

An **empty** `If-Match` means *"only if the key does not exist"* — a create-only
write.

> **curl trap.** `-H 'If-Match: '` with a trailing space makes curl *drop* the
> header, turning a conditional write into an unconditional one that returns
> `204`. To send the header empty you must write `-H 'If-Match;'` with a
> semicolon. This is curl's behaviour, not the gateway's, and it is very easy to
> mistake for a bug in the conditional write.

### Watching for changes

```bash
curl -N 'localhost:8137/kv?watch=1&prefix=app/'
```

Server-sent events: a `snapshot` of the store as it stands, then one `kv` event
per change, filtered to the prefix. A watch is a read, so a read-only gateway
still serves it.

### Locking the gateway down

```bash
-http-token s3cret     # every request needs it, /health and /metrics included
-http-readonly         # refuse every mutating request (403); reads and watches still work
-http-tls-cert / -http-tls-key   # serve it over TLS
```

The token goes in `Authorization: Bearer <token>` or `X-Rezoagwe-Token`. There is
deliberately no exemption for health checks.

---

## 6. `rezoagwectl`

```bash
export REZOAGWE_SERVER=http://127.0.0.1:8137   # not -url; the flag is -server
```

```bash
rezoagwectl put config/mode fast
rezoagwectl get config/mode
echo 'a long value' | rezoagwectl put config/notes    # reads stdin when the value is omitted
rezoagwectl -ttl 60s put session/x tok
rezoagwectl -if-match '*' put config/mode slow        # only if absent
rezoagwectl ls app/
rezoagwectl watch app/
rezoagwectl history config/mode
rezoagwectl del config/mode

rezoagwectl peers | health | topology | doctor | check
rezoagwectl export backup.json
rezoagwectl import backup.json
rezoagwectl say 'deploying now'
rezoagwectl chat
```

Exit codes are meant to be branched on: `0` ok, `1` error, `2` no such key,
`3` version mismatch, `4` replicas disagree.

> `ls` prints `key<TAB>value`, not just keys — piping `ls` with a 2 MB value in
> the store will print the 2 MB. Use a prefix.

---

## 7. The desktop client

```bash
make electron
```

**Settings** first: nickname, node port, advertise host, bootstrap seeds,
cluster name, pre-shared key — then **Apply**. The node restarts whenever the
port, cluster, key, advertise host or seeds change, because those are baked into
the socket and the codec.

- Leave **advertise host** blank on a normal LAN; fill it in behind a port
  forward, or when Diag tells you which one to use.
- **Seeds** is a comma-separated list. A node whose first seed is dead joins
  through the second.
- A value the form cannot keep — a port outside 1–65535, a blank nickname or
  cluster — is refused, the field snaps back to what is actually in effect, and
  a warning says which value was dropped. Spaces around the key and cluster name
  are trimmed silently and on purpose: a pasted key that brings a newline along
  would otherwise cut the node off from every peer with nothing on screen to
  show why.

The screens: **Keys**, **Chat**, **Peers**, **Graph**, **Activity**, **Diag**,
**Bootstrap**, **Settings**. The desktop can run the rendezvous service itself —
turn it on under **Bootstrap** or tick *Start the rendezvous service
automatically* — which is enough for a small LAN with no server.

---

## 8. The Android app

Install:

```bash
adb install -r dist/rezoagwe-<version>-android-debug.apk
```

Accept the notification permission when asked: the node runs as a foreground
service and that is the notification it needs.

**Settings** → nickname, node port, advertise host, bootstrap seeds, cluster
name, pre-shared key → **Apply** (the button is below the fold; scroll).

Then **Peers** → **Start node**. The status bar turns `CONNECTED` and shows the
peer, key and tombstone counts.

What to know:

- **Leave *advertise host* blank.** The app fills in the device's own Wi-Fi
  address, and re-detects it when the network changes.
- **The node keeps running when you leave the app** — it is a foreground
  service. It survives backgrounding, a recents swipe and deep Doze. It also
  restarts by itself after a force-stop or a reboot, because the app remembers
  that you asked for it to be up.
- **Stop node** announces the departure, so peers drop it in a few seconds
  rather than waiting for the eviction timer.
- The app can be the rendezvous service too: **Bootstrap** → **Start**. Other
  nodes then point at `<phone-ip>:9999`.
- Rotating the device keeps what you have typed — an open key dialog, a chat
  draft, unsaved settings. Navigating *away* from Settings before pressing Apply
  still discards the edit.
- **Diag** is the screen to open when something is wrong. With Wi-Fi off it says
  so first and explains that the send errors and missing peers below follow from
  that, rather than sending you to check the bootstrap and the key.

---

## 9. When something is wrong

Ask the node, in this order.

**1. Do the two nodes even share a key?** Compare the **key fingerprint** on
both — TUI `D`, desktop and Android **Diag**, or `GET /diagnostics`. Different
fingerprints means a different cluster name or pre-shared key, and nothing else
needs investigating until they match.

**2. What does the node itself say?**

```bash
./dist/rezoagwe-discovery -doctor -bootstrap … -cluster … -psk …
curl localhost:8137/diagnostics?format=text
rezoagwectl doctor
```

`-doctor` starts a node, waits out two heartbeats, prints a full report and
exits. The waiting matters: a node seconds old has not failed to do anything
yet.

Findings are written as what to do about them, not as counters. The ones worth
knowing in advance:

| Finding | What it actually means |
|---|---|
| *Advertising an address the cluster does not see this node at* | the rendezvous saw you elsewhere; it names the address to use |
| *Advertising a loopback address* | usually: you are not on a network. Reconnect |
| *This device has no network* (Android) | everything below it follows from that |
| *N packet(s) failed authentication* | something nearby frames packets with a different key |
| *N remote update(s) refused by this node's limits* | `-max-keys` / `-max-value-bytes` are too small for what the cluster holds; this node can never converge on those keys, and waiting will not fix it |
| *State file could not be read* | the node started empty **and with a new identity**, and has already rewritten the file; its old writes are outranked and its old chat lines now read as another author's |
| *N peer(s) never answered* | one-way UDP — a firewall or a wrong advertised port, not packet loss |
| *N tombstone(s) kept forever* | informational, unless deletes are frequent |

**3. Do the replicas actually agree?**

```bash
curl localhost:8137/consistency        # or `v` in the TUI, *Verify replicas* on Android
```

Each peer is asked for a fold of its own store and the answers are compared
bucket by bucket. `agrees: false` names the keys that differ. This is the check
to trust over "it looks fine" — it folds the values, not just the versions, so
two replicas holding different data at the same version are caught.

**4. If a replica is genuinely wrong**, make a known-good node's copy win:

```bash
rezoagwectl repair app/config app/secret
rezoagwectl repair -divergent          # every key this node's own check calls divergent
```

> This overwrites those keys on **every** replica. The keys have to be named, or
> derived from this node's consistency check, because "repair everything" run on
> the wrong node overwrites a cluster with stale data and nothing in the protocol
> can tell which side was right.

---

## 10. Backups

```bash
rezoagwectl export backup.json          # or GET /export
rezoagwectl import backup.json          # merge: normal last-write-wins rules
rezoagwectl -seed import backup.json    # seed: the import wins outright
```

Merge is the safe default and the only one you want on a running cluster. Seed
exists for restoring into an empty one.

For unattended backups, have the node write them itself:

```bash
-snapshot-dir /var/backups/rezoagwe -snapshot-interval 1h -snapshot-keep 24
```

> The state file is a **replica, not a backup**: it is rewritten continuously, so
> it follows a mistake as fast as everything else. Snapshots and exports are the
> backup.

---

## 11. Things that will surprise you

Every one of these was observed on real hardware.

**A deleted key can come back if you enable tombstone GC.** A delete replicates
as a tombstone. `-tombstone-ttl` reclaims old ones — and a node that was offline
longer than that still holds the value, and pushes it back once the tombstone is
gone, undoing the delete on every replica. If you set a GC age, it has to exceed
the longest outage you expect a node to return from. With GC off (the default)
this cannot happen.

**A node restarted without its state file is a new node.** Identity lives in the
state file. Delete it, or point `-data` somewhere new, and the node comes back
with a new id: a new chat author, and writes that sort differently against its
own older ones. `-data -` disables persistence entirely, which is right for a
throwaway node and wrong for anything else.

**Two nodes must never start from copies of one state file.** They share an
identity, so their writes land at identical versions, neither is newer, and
neither replica ever accepts the other. They diverge permanently. Both report it
under Diag, and the fix is to stop one and delete its state file.

**A node whose store limits are too small never converges.** `-max-keys` and
`-max-value-bytes` refuse a peer's update, which is a deliberate divergence, not
a transient one. The node counts it apart from staleness and raises an error
saying waiting will not help.

**A chat line is recoverable only while its author still holds it.** Chat is
sequenced per author and gaps are repaired by asking the author. If the line has
rotated out of their ring, it is reported lost by name rather than silently
missing — every line is either delivered or named.

**Values must be valid UTF-8.** Bodies are JSON on the wire, and an invalid byte
would be silently replaced, making two replicas differ at the same version
forever. A write like that is refused with `422` at the door. Keys and values are
otherwise arbitrary: newlines, tabs, emoji, 2 MB values and 4 KB keys all
replicate and fold identically across the Go, Kotlin and JavaScript
implementations.

**Clocks matter.** Packets carry a timestamp and are refused outside a ±30 s
window. A node whose clock is badly wrong is deaf, and Diag names clock skew
separately from a wrong key.

**The pre-shared key separates clusters; it does not make you anonymous.**
Anyone with the key can impersonate any node. See DESIGN.md §11.

---

## 12. Quick reference

### `rezoagwe-discovery`

| Flag | Default | Meaning |
|---|---|---|
| `-node` | `:3137` | address to bind |
| `-advertise` | `-node` | address peers should use; `auto` asks the rendezvous |
| `-bootstrap` | `:9999` | rendezvous address, or a comma-separated list of seeds |
| `-cluster` | `rezoagwe` | cluster name; nodes only talk to their own |
| `-psk` | — | pre-shared key authenticating every packet |
| `-nick` | `anon` | chat nickname |
| `-data` | config dir | state file; `-` disables persistence |
| `-http` | — | serve the HTTP gateway here, e.g. `:8080` |
| `-http-token` | — | require this bearer token on every request |
| `-http-readonly` | `false` | refuse every mutating request |
| `-http-tls-cert` / `-http-tls-key` | — | serve the gateway over TLS |
| `-max-value-bytes` | `0` | refuse values longer than this (0 = no limit) |
| `-max-keys` | `0` | refuse writes past this many live keys (0 = no limit) |
| `-tombstone-ttl` | `0` | reclaim tombstones older than this (0 = never) |
| `-snapshot-dir` | — | write periodic store exports here |
| `-snapshot-interval` | `1h` | how often |
| `-snapshot-keep` | `24` | how many to retain |
| `-mdns` | `false` | find peers on the LAN with no seeds configured |
| `-headless` | `false` | no TUI; pair with `-http` |
| `-doctor` | `false` | start, join, print a diagnostics report, exit |
| `-logfile` | — | write logs here instead of discarding them |
| `-debug` | `false` | debug logging (needs `-logfile`) |
| `-version` | — | print the version and exit |

### `rezoagwe-bootstrap`

| Flag | Default | Meaning |
|---|---|---|
| `-port` | `9999` | listen port (UDP and TCP) |
| `-timeout` | `30s` | drop a node that has not registered within this window |
| `-data` | config dir | roster file; `-` disables |
| `-psk` | — | must match the nodes |
| `-cluster` | `rezoagwe` | must match the nodes |
| `-headless` | `false` | no TUI |
| `-logfile` / `-debug` / `-version` | — | as above |

### Where to look next

- [README.md](README.md) — install, package, cross-compile
- [DESIGN.md](DESIGN.md) — the protocol, and why each decision went the way it did
- [android/README.md](android/README.md) — the Android app in detail, and what hardware testing has found
- [electron/README.md](electron/README.md) — the desktop client in detail
- [ROADMAP.md](ROADMAP.md) — what has shipped and what is next
