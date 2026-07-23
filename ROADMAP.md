# Rezoagwe — Roadmap

Prioritized backlog for hardening the PoC. Items are numbered by **global
priority** (1 = do next) and grouped into thematic tiers. Effort is a rough
T-shirt size: **S** ≈ a few hours, **M** ≈ about a day, **L** ≈ multi-day.

This complements the *Known limitations & next steps* table in
[DESIGN.md](DESIGN.md#6-known-limitations--next-steps); each open item notes
the code it touches.

## Shipped

Recent milestones, newest first — context for what the backlog builds on:

- ✅ **Versioned last-write-wins** — per-key `Version` (Lamport counter + node
  tiebreak); `KindKV` carries JSON `KVUpdate`; tombstoned deletes; state-sync
  merges by version; versions + clock persisted.
- ✅ **Robust bootstrap join** — bounded `DiscoverNodes` read + background
  rejoin; no more startup hang when bootstrap is down/lossy.
- ✅ **Chat-history sync on join** — a joiner prepends the peer's recent chat.
- ✅ **Local persistence** — atomic, generation-guarded JSON store (`-data`).
- ✅ **Graceful leave** — `Goodbye` on `Ctrl+Q` for instant peer eviction.

---

## Tier 1 — Correctness: finish "eventually consistent"

### 1. Anti-entropy reconciliation — **M**
Periodically exchange a per-key version digest on the gossip tick and pull
whatever is missing or stale. **Why:** the last correctness gap — a dropped
UDP `KVUpdate` is currently never re-sent, so two stores stay divergent until
the next overlapping write. Versioning already did the hard part (deterministic
newer-wins merge via `KVStore.Apply`), so this is now safe and mechanical.
**Notes:** builds on `Version`/`Apply`; a related enhancement is pulling
state-sync from a *quorum* of peers rather than one random peer. Maps to the
Replication + State-sync rows in DESIGN §6.

### 2. Chunked / TCP state sync — **M**
Stop shipping the whole snapshot (KV entries + chat) in one UDP datagram.
**Why:** a store/chat larger than ~64 KB silently fails to sync — the joiner
gets nothing. **Notes:** either length-prefix and chunk over UDP with acks, or
open a short-lived TCP stream for `StateRequest`/`StateResponse`. Also fixes
the bootstrap-roster ceiling (still a single datagram). Maps to the Transport /
State-sync-size / Bootstrap-roster rows in DESIGN §6.

## Tier 2 — Security & networking

### 3. Pre-shared key + HMAC auth — **S–M**
Tag every packet with an HMAC over a pre-shared key; drop unauthenticated
packets. **Why:** today anyone on the network can inject KV writes, chat, and
bogus peers. Far cheaper than DTLS and closes the Security row in DESIGN §6.
**Notes:** wrap `sendTo` / `HandleConnection`; reject on bad tag before
dispatch.

### 4. Multiple bootstrap seeds (and/or mDNS) — **S**
Accept a comma-separated `-bootstrap a,b,c` and try each; optionally discover
peers via mDNS on a LAN. **Why:** bootstrap is a single point of failure for
new joiners. **Notes:** loop over seeds in `DiscoverNodes`; the rejoin loop
already retries.

## Tier 3 — Operability & hygiene

### 5. Real diagnostics — **S**
Add a `-debug` flag that raises the logrus level **and** redirects logs to a
file. **Why:** `log.Debugf` is currently inert (level never raised), and
logging to stderr would scribble over the tview screen — so there is no usable
way to get logs today. Maps to the Diagnostics row in DESIGN §6.

### 6. Single-source the version — **S**
Inject the version via `-ldflags "-X main.version=$(VERSION)"` instead of the
hardcoded `v0.0.3` in both controllers. **Why:** `make VERSION=x` renames the
output files but the running binary still reports the old version.

### 7. Tombstone GC — **M**
Reclaim deleted-key tombstones once their deletion is cluster-wide certain
(e.g. age + observed by all live peers). **Why:** tombstones currently
accumulate forever in memory and on disk. **Notes:** needs care not to reopen
the resurrection hole anti-entropy (#1) closes; do this after #1.

## Tier 4 — Capabilities & reach

### 8. HTTP/REST gateway — **M**
Optional `-http :8080` exposing `GET/PUT/DELETE /kv/{key}`. **Why:** makes the
store scriptable and embeddable — and end-to-end testable without driving the
TUI. High leverage for a KV store.

### 9. Key TTL / expiry — **S–M**
Optional per-key TTL plus a sweeper (Redis-style `EX`). **Notes:** pairs
naturally with the version stamp; expiry is just a tombstone with a future
effective time.

### 10. Chat enhancements — **S each**
Runtime `/nick` rename (nickname is flag-only at startup today), `/me`, and
direct messages over the existing per-peer send path.

### 11. Persist the chat ring — **S**
Persist chat to disk alongside the KV store. **Why:** chat is currently synced
from peers on join but lost on restart if the node is alone. Maps to the
Chat-history row in DESIGN §6.

## Tier 5 — Minor cleanups

### 12. Reuse one send socket — **S**
`sendTo` opens and closes a fresh UDP socket per packet; reuse the listener or
a cached dialer, and fix the misleading "shared sender + receiver" comment on
the `listen` field.

### 13. Validate learned peer addresses — **S**
Reject malformed addresses from gossip so garbage entries don't linger in the
node list until eviction.

### 14. Prune vestigial protobuf — **S**
`Payload` / `DiscoveryAction` in `rezoagwe.proto` are unused now that KV moved
to JSON; remove them (and regenerate) so the proto reflects reality — protobuf
is only the bootstrap handshake now.
