package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The graph is assembled from peer lists that only exist because gossip
// carried them between processes, which is the half the in-process tests
// cannot see.
func TestTheClusterGraphSeesEveryNode(t *testing.T) {
	g := a.topology(t)

	if len(g.Nodes) < len(cluster) {
		t.Fatalf("graph has %d nodes, want at least the %d in the cluster: %+v",
			len(g.Nodes), len(cluster), g.Nodes)
	}
	var self int
	for _, n := range g.Nodes {
		if n.Role == "self" {
			self++
		}
	}
	if self != 1 {
		t.Fatalf("graph has %d nodes marked self, want exactly 1", self)
	}
	if len(g.Links) == 0 {
		t.Fatal("a formed cluster produced no links")
	}
}

// A healthy cluster has to look like one connected component. More than one is
// a partition — the failure the graph exists to make visible.
func TestTheClusterIsNotPartitioned(t *testing.T) {
	for _, n := range core {
		d := n.diagnostics(t)
		for _, c := range d.Checks {
			if strings.Contains(c.Title, "partitioned") {
				t.Fatalf("%s reports a partition: %s", n.name, c.Detail)
			}
		}
	}
}

// Peers this node exchanges packets with must be links, not hearsay: a
// direct-role node with a degree of zero means gossip arrived and membership
// did not.
func TestPeersAppearAsLinkedNodes(t *testing.T) {
	g := a.topology(t)
	for _, n := range g.Nodes {
		if n.Role == "direct" && n.Degree == 0 {
			t.Fatalf("peer %s is in the graph with no links: %+v", n.Addr, n)
		}
	}
}

// The rules have to agree with a cluster that is demonstrably working: every
// other test in this suite passed against it.
func TestDiagnosticsFindNothingWrongWithAWorkingCluster(t *testing.T) {
	for _, n := range core {
		d := n.diagnostics(t)
		if len(d.Checks) == 0 {
			t.Fatalf("%s produced no checks at all", n.name)
		}
		for _, c := range d.Checks {
			if c.Severity == "error" {
				t.Errorf("%s reports an error against a working cluster: %s — %s",
					n.name, c.Title, c.Detail)
			}
		}
	}
}

// There is deliberately no end-to-end test that a stranger provokes the
// "failed authentication" finding: the stranger containers only ever talk to
// the rendezvous, so no peer hears them and the assertion could only ever
// skip. The rule itself is covered by TestAuthFailuresBlameTheKey in
// pkg/discovery/diagnostics.

func TestTheTextReportIsPasteable(t *testing.T) {
	code, _, body := a.do(t, http.MethodGet, "/diagnostics?format=text", "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /diagnostics?format=text = %d", code)
	}
	for _, want := range []string{"rezoagwe node diagnostics", "peers (", "topology:", "checks"} {
		if !strings.Contains(body, want) {
			t.Fatalf("report missing %q:\n%s", want, body)
		}
	}
}

// The claim the consistency check exists to make, asserted between real
// processes: after the suite has written to this cluster, every replica holds
// the same store.
func TestEveryReplicaAgreesOnTheStore(t *testing.T) {
	key := "consistency-probe"
	a.put(t, key, "value")
	valueEverywhere(t, cluster, key, "value", envSeconds("E2E_CONVERGE_SEC", 20))

	eventually(t, "every replica to agree", envSeconds("E2E_CONVERGE_SEC", 20), func() (bool, string) {
		r := a.consistency(t)
		if r.Converged {
			return true, ""
		}
		var who []string
		for _, p := range r.Peers {
			if p.Reachable && !p.Agrees {
				who = append(who, p.Addr)
			}
		}
		return false, "still differing: " + strings.Join(who, ", ")
	})
}

// A peer that answers has to be reported as reachable and counted, or a report
// full of silence would read as agreement.
func TestConsistencyReachesEveryPeer(t *testing.T) {
	r := a.consistency(t)
	if len(r.Peers) == 0 {
		t.Fatal("no peers in the consistency report")
	}
	if r.Unreachable > 0 {
		var who []string
		for _, p := range r.Peers {
			if !p.Reachable {
				who = append(who, p.Addr+" ("+p.Error+")")
			}
		}
		t.Fatalf("%d peer(s) did not answer over a stream: %s", r.Unreachable, strings.Join(who, ", "))
	}
	if r.Keys == 0 {
		t.Fatal("the reporting node claims an empty store after the suite wrote to it")
	}
	for _, p := range r.Peers {
		if p.Keys == 0 {
			t.Errorf("peer %s reports an empty store", p.Addr)
		}
	}
}

// Levels have to reach the exposition, or nothing can be alerted on.
func TestGaugesAreExposed(t *testing.T) {
	a.put(t, "gauge-probe", "x")
	for _, series := range []string{"rezoagwe_keys", "rezoagwe_peers", "rezoagwe_value_bytes"} {
		if v := a.metric(t, series); v <= 0 {
			t.Errorf("%s = %v, want a positive level", series, v)
		}
	}
}

// Per-peer counters are the pair that separates a quiet peer from an
// unreachable one, and they only mean anything across real sockets.
func TestPerPeerTrafficIsCountedInBothDirections(t *testing.T) {
	code, _, body := a.do(t, http.MethodGet, "/metrics", "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /metrics = %d", code)
	}
	if !strings.Contains(body, "rezoagwe_peer_packets_sent_total{peer=") {
		t.Fatal("no per-peer sent series in the exposition")
	}
	if !strings.Contains(body, "rezoagwe_peer_packets_received_total{peer=") {
		t.Fatal("no per-peer received series in the exposition")
	}
}

// The ETag was already published; a poller that sends it back must be told
// nothing changed rather than handed the value again.
func TestConditionalReadIsNotModified(t *testing.T) {
	key := "etag-probe"
	etag := a.put(t, key, "one")
	if etag == "" {
		t.Fatal("PUT returned no ETag")
	}
	code, _, _ := a.do(t, http.MethodGet, "/kv/"+key, "", map[string]string{"If-None-Match": etag})
	if code != http.StatusNotModified {
		t.Fatalf("conditional GET = %d, want 304", code)
	}
	a.put(t, key, "two")
	code, _, _ = a.do(t, http.MethodGet, "/kv/"+key, "", map[string]string{"If-None-Match": etag})
	if code != http.StatusOK {
		t.Fatalf("GET after a write = %d, want 200", code)
	}
}

// ---- refusing a write ------------------------------------------------------
//
// Against node-solo, which is alone in a cluster of its own with limits small
// enough to trip in one request. Refusing a write is a gateway and store
// property rather than a cluster one, and testing it here keeps the statuses
// away from everything that asserts the real cluster agrees.

// A write the store refused must not be reported as a write.
//
// Set's result was discarded in the handler, so a value over the limit came
// back 204 with an ETag of the zero version: the client was told it had
// succeeded and had no way to learn otherwise. Nothing in a unit test for the
// store catches that — the store was right, the handler threw the answer away.
func TestAValueOverTheNodeLimitIsRefused(t *testing.T) {
	code, body := solo.putStatus(t, "too-big", strings.Repeat("x", 200))
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a value over -max-value-bytes = %d, want 413: %s", code, body)
	}
	if !strings.Contains(body, "size limit") {
		t.Errorf("the refusal does not say which rule refused it: %s", body)
	}
	if _, _, ok := solo.get(t, "too-big"); ok {
		t.Error("the refused value was stored anyway")
	}
}

// The key limit needs its own status: "try a smaller value" is the wrong
// advice when the value was never the problem.
func TestAWriteBeyondTheKeyLimitSaysTheStoreIsFull(t *testing.T) {
	for i := 0; i < 3; i++ {
		if code, body := solo.putStatus(t, fmt.Sprintf("fill-%d", i), "v"); code != http.StatusNoContent {
			t.Fatalf("filling key %d = %d, want 204: %s", i, code, body)
		}
	}
	code, body := solo.putStatus(t, "one-too-many", "v")
	if code != http.StatusInsufficientStorage {
		t.Fatalf("a key past -max-keys = %d, want 507: %s", code, body)
	}
	if !strings.Contains(body, "key limit") {
		t.Errorf("the refusal does not say which rule refused it: %s", body)
	}
}

// A body past the gateway's own cap is refused, not quietly shortened.
//
// io.LimitReader stopped at the limit and reported no error, so a PUT one byte
// over stored the first 32 MB and answered 204 — the cluster then replicating
// a prefix of what the client sent, with nothing anywhere saying so. The whole
// body has to cross a real socket for this to mean anything, which is why it
// is here rather than in a handler test.
func TestABodyOverTheGatewayCapIsRefusedNotTruncated(t *testing.T) {
	const cap = 32 << 20
	code, _ := solo.putStatus(t, "over-the-cap", strings.Repeat("y", cap+1024))
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a body over the %d-byte cap = %d, want 413", cap, code)
	}
	if _, _, ok := solo.get(t, "over-the-cap"); ok {
		t.Error("a truncated body was stored anyway")
	}
}

// A lost compare-and-swap keeps its own status: it means "someone got there
// first", which is a different thing to tell a caller holding a lock than
// "that will never fit".
func TestARefusedSwapIsStillAPreconditionFailure(t *testing.T) {
	// A key the previous test already created: the store is at its key limit
	// by now, and an update to a key already held is always allowed, so this
	// tests the swap rather than the limit.
	const key = "fill-0"
	if _, _, ok := solo.get(t, key); !ok {
		t.Fatalf("%s should already exist from the key-limit test", key)
	}
	code, _ := solo.putIfMatch(t, key, "other", "1.nobody")
	if code != http.StatusPreconditionFailed {
		t.Fatalf("a swap against a stale version = %d, want 412", code)
	}
	// And the value is untouched.
	if v, _, _ := solo.get(t, key); v != "v" {
		t.Errorf("the refused swap changed the value to %q", v)
	}
}

// ---- naming a divergence ---------------------------------------------------
//
// Last on purpose. This is the one test that deliberately leaves the cluster
// disagreeing, so everything that asserts the replicas agree has already run.

// "They differ somewhere in bucket 7" is where a report used to stop, which
// on a store of a few hundred keys is a sixteenth of the keyspace to go and
// read by hand. The check knows which key it is; it just never said.
//
// The divergence is the real one: node-limited is configured tighter than its
// peers, so a value the others accept is one it refuses — a deliberate split
// that nothing on the wire reports, which is exactly what the consistency
// check is for.
func TestAConsistencyReportNamesTheDivergingKey(t *testing.T) {
	const key = "beyond-graces-limit"
	// Comfortably over node-limited's ceiling and comfortably under
	// everyone else's, so precisely one node refuses it.
	a.put(t, key, strings.Repeat("z", 2<<20))

	// Everyone but node-limited takes it.
	for _, n := range []node{a, b, c, late, restart} {
		n := n
		eventually(t, n.name+" to accept the large value", converge, func() (bool, string) {
			if _, _, ok := n.get(t, key); ok {
				return true, ""
			}
			return false, n.name + " has not got it yet"
		})
	}

	var found peerConsistency
	eventually(t, "the report to name the key node-limited refused", converge, func() (bool, string) {
		r := a.consistency(t)
		if r.Converged {
			return false, "the cluster still claims to agree"
		}
		for _, p := range r.Peers {
			if !strings.HasPrefix(p.Addr, "node-limited") {
				continue
			}
			if p.Agrees {
				return false, "node-limited still claims to agree"
			}
			if len(p.Differences) == 0 {
				return false, fmt.Sprintf("node-limited differs in %d bucket(s) but names no key",
					len(p.DifferingBuckets))
			}
			found = p
			return true, ""
		}
		return false, "node-limited is not in the report"
	})

	var named *keyDifference
	for i := range found.Differences {
		if found.Differences[i].Key == key {
			named = &found.Differences[i]
		}
	}
	if named == nil {
		t.Fatalf("the report names %d key(s) but not %s: %+v", len(found.Differences), key, found.Differences)
	}
	// Present here, absent there — the direction that says a write never
	// arrived, and the one a walk over the local store alone cannot find.
	if named.Local == "" {
		t.Errorf("%s: this node holds the key, so its version should be named", key)
	}
	if named.Remote != "" {
		t.Errorf("%s: node-limited refused the value, so it should be reported absent, got %q",
			key, named.Remote)
	}

	// And the status still carries the verdict on its own, for a script that
	// reads nothing else.
	code, _, _ := a.do(t, http.MethodGet, "/consistency", "", nil)
	if code != http.StatusConflict {
		t.Errorf("GET /consistency over a divergent cluster = %d, want 409", code)
	}
}

// A replica that will not hold what a peer sends it has to say so.
//
// node-limited sits in the real cluster with a value ceiling, so a large
// enough write from node-a is one every other replica keeps and it refuses.
// That refusal used to be counted as a stale rejection and narrated as
// "ignored stale set for <key>" — for a key it had never held — which left the
// one node in the cluster that could never converge reporting itself healthy.
// Only a second process can produce this: a refusal arrives over the wire, and
// the local write path reports it to its caller instead.
func TestARefusedReplicationIsNotReportedAsStaleness(t *testing.T) {
	staleBefore := limited.metric(t, `rezoagwe_kv_rejected_stale_total`)
	refusedBefore := limited.metric(t, `rezoagwe_kv_refused_total`)

	// Comfortably past node-limited's -max-value-bytes, and comfortably under
	// the gateway's body cap, so only the replica's own limit refuses it.
	key := "refused-by-the-limited-replica"
	a.put(t, key, strings.Repeat("x", 1_200_000))

	eventually(t, "node-limited counts the refusal", 30*time.Second, func() (bool, string) {
		got := limited.metric(t, `rezoagwe_kv_refused_total`)
		return got > refusedBefore, fmt.Sprintf("kv_refused_total = %v, want > %v", got, refusedBefore)
	})

	// And it did not land in the counter that means "last-write-wins resolved
	// a conflict", which is the counter nothing ever alerts on.
	if got := limited.metric(t, `rezoagwe_kv_rejected_stale_total`); got != staleBefore {
		t.Fatalf("kv_rejected_stale_total moved from %v to %v: a refusal was counted as staleness",
			staleBefore, got)
	}

	// The whole point is that the node now says it is in trouble.
	eventually(t, "the diagnostics name the refusals", 20*time.Second, func() (bool, string) {
		d := limited.diagnostics(t)
		for _, c := range d.Checks {
			if strings.Contains(c.Title, "refused") {
				if c.Severity != "error" {
					return false, fmt.Sprintf("severity = %q, want error", c.Severity)
				}
				if !strings.Contains(c.Detail, "not stale") {
					return false, fmt.Sprintf("detail does not say waiting will not fix it: %q", c.Detail)
				}
				return true, ""
			}
		}
		return false, fmt.Sprintf("no check mentions the refusals: %+v", d.Checks)
	})

	// The peers that did take it stay divergent from this one, which is what
	// the error is warning about.
	valueEverywhere(t, []node{a, b}, key, strings.Repeat("x", 1_200_000), 30*time.Second)
}

// Chat delivery is reported, and the numbers add up across real processes.
//
// A chat line has no digest behind it the way a key does, so the only evidence
// a line arrived is the sequence it carries. In-process tests can assert the
// tracker's rules; only separate processes exchanging real datagrams can show
// that the sequence survives the wire, that an author answers a pull for its
// own lines, and that nothing is counted as missing on a healthy cluster.
func TestChatIsSequencedAcrossProcesses(t *testing.T) {
	line := fmt.Sprintf("sequenced-%d", time.Now().UnixNano())
	a.say(t, line)

	for _, n := range []node{b, c} {
		eventually(t, n.name+" receives the line", 20*time.Second, func() (bool, string) {
			for _, e := range n.chat(t) {
				if e.Text == line {
					return true, ""
				}
			}
			return false, "not in the log yet"
		})
	}

	// Every peer agrees nothing is missing: a cluster that is working must not
	// report gaps, or the finding means nothing when it does appear.
	for _, n := range []node{a, b, c} {
		if got := n.metric(t, `rezoagwe_chat_lost_total`); got != 0 {
			t.Fatalf("%s reports %v chat line(s) lost on a healthy cluster", n.name, got)
		}
		if got := n.metric(t, `rezoagwe_chat_duplicates_total`); got != 0 {
			t.Fatalf("%s counted %v duplicate chat line(s) with nothing re-sending them", n.name, got)
		}
	}

	// And the line is held once, not twice — the sequence is what makes a
	// re-delivery recognisable, and a cluster with no loss should never have
	// exercised that path at all.
	seen := 0
	for _, e := range b.chat(t) {
		if e.Text == line {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("the line appears %d times in %s's log, want 1", seen, b.name)
	}
}

// A node that joins after a conversation has started must not report every
// line it was not there for as lost. A joiner that cried wolf would make the
// finding useless on the one cluster where it matters.
func TestAJoinerDoesNotReportTheConversationItMissedAsLost(t *testing.T) {
	if got := late.metric(t, `rezoagwe_chat_lost_total`); got != 0 {
		t.Fatalf("the late joiner reports %v chat line(s) lost that were said before it arrived", got)
	}
	if got := late.metric(t, `rezoagwe_chat_gaps_total`); got != 0 {
		t.Fatalf("the late joiner claims %v gap(s) in a conversation it was not in", got)
	}
}

// A restarted node has to carry on numbering where it left off, and this is
// the only level that can show it: the hazard needs a second process using the
// same identity, and a peer that still remembers the first one.
//
// The sequence is what makes a re-delivery recognisable, so a node that began
// counting again at 1 would have its first lines read as duplicates of the
// ones its peers already hold — and **dropped**. The feature built to stop
// chat going missing would be the thing making it go missing, silently, on
// every restart.
func TestARestartedNodeContinuesItsChatSequence(t *testing.T) {
	ensureRestarted(t, restart, envSeconds("E2E_RESTART_SEC", 30)+settle)
	eventually(t, "the restarted node to rejoin", settle, func() (bool, string) {
		if h := restart.health(t); !h.Connected {
			return false, fmt.Sprintf("peers = %d", h.Peers)
		}
		return true, ""
	})

	before := findChat(restart.chat(t), restartChatBefore)
	if before == nil {
		t.Skip("the pre-restart line never landed before the first process was killed")
	}
	if before.Seq == 0 {
		t.Fatalf("the pre-restart line carries no sequence: %+v", before)
	}

	after := fmt.Sprintf("frank is back %d", time.Now().UnixNano())
	restart.say(t, after)

	// The consequence first: the line reaches every peer. A sequence that
	// restarted at 1 would be refused as something they already hold, and this
	// is the assertion that would fail — with nothing in any log to say why.
	for _, n := range []node{a, b, c} {
		eventually(t, n.name+" receives the line said after the restart", settle, func() (bool, string) {
			if countText(n.chat(t), after) == 0 {
				return false, "not in the log yet"
			}
			return true, ""
		})
	}

	// Then the cause: the numbering continued rather than starting over.
	sent := findChat(restart.chat(t), after)
	if sent == nil {
		t.Fatal("the restarted node does not hold its own line")
	}
	if sent.Seq <= before.Seq {
		t.Fatalf("sequence went from %d to %d across the restart: the counter did not survive, "+
			"so every peer reads these lines as duplicates it already has",
			before.Seq, sent.Seq)
	}
	if sent.Node != before.Node {
		t.Fatalf("node id changed across a restart with a state file: %q -> %q", before.Node, sent.Node)
	}

	// And exactly once everywhere — the pull path and a state snapshot can both
	// deliver a line, so recognising the second copy is what keeps it to one.
	for _, n := range []node{a, b, c, restart} {
		if got := countText(n.chat(t), after); got != 1 {
			t.Errorf("%s holds the line %d times, want 1", n.name, got)
		}
	}
}

// findChat returns the first entry with this text, or nil.
func findChat(entries []chatEntry, text string) *chatEntry {
	for i := range entries {
		if entries[i].Text == text {
			return &entries[i]
		}
	}
	return nil
}

// A repair reply may only fill a gap the asking node is already waiting for.
//
// Found on hardware: a backlog carrying a line attributed to another peer,
// numbered far ahead, was accepted — which raised that author's high-water
// mark so far that every genuine line it sent afterwards sorted below it and
// was dropped as a duplicate. One datagram silenced an author at that node
// permanently, through the mechanism built to stop chat going missing. Only
// separate processes can show it: the guard is on what arrives over the wire.
func TestChatRepairNeverIntroducesState(t *testing.T) {
	line := fmt.Sprintf("guarded-%d", time.Now().UnixNano())
	a.say(t, line)
	for _, n := range []node{b, c} {
		eventually(t, n.name+" receives the line", 20*time.Second, func() (bool, string) {
			return countText(n.chat(t), line) == 1, "not in the log yet"
		})
	}

	// On a cluster where nothing was lost, nothing was ever asked for — so a
	// healthy run must show no backlog traffic and nothing refused either.
	for _, n := range []node{a, b, c} {
		if got := n.metric(t, `rezoagwe_chat_unsolicited_total`); got != 0 {
			t.Errorf("%s refused %v backlog line(s) on a cluster that lost nothing", n.name, got)
		}
		if got := n.metric(t, `rezoagwe_chat_self_attributed_total`); got != 0 {
			t.Errorf("%s was offered %v line(s) claiming to be its own", n.name, got)
		}
	}
}

// Every peer's heartbeat says where its own numbering stands, which is what
// makes a quiet author's lines recoverable at all: without it the baseline
// waits until that author speaks, and anything said before then is neither
// delivered nor reported. Measured on hardware before the fix: three lines
// from a peer that had never spoken, gone in silence.
func TestEveryPeerKnowsWhereEveryAuthorsNumberingStands(t *testing.T) {
	// Give every node something to announce, then let a heartbeat round pass.
	a.say(t, fmt.Sprintf("a-speaks-%d", time.Now().UnixNano()))
	b.say(t, fmt.Sprintf("b-speaks-%d", time.Now().UnixNano()))
	time.Sleep(8 * time.Second)

	// Nobody may be claiming a gap: every author's position is known and
	// every line was delivered.
	for _, n := range []node{a, b, c} {
		d := n.diagnostics(t)
		for _, chk := range d.Checks {
			if strings.Contains(chk.Title, "chat line") {
				t.Errorf("%s reports %q on a cluster that lost nothing", n.name, chk.Title)
			}
		}
		if got := n.metric(t, `rezoagwe_chat_lost_total`); got != 0 {
			t.Errorf("%s gave up on %v chat line(s)", n.name, got)
		}
	}
}

// Two nodes started from copies of one state file share a node id, so their
// writes to one key are stamped with the same counter *and* the same id.
// Neither version is newer, so neither replica ever accepts the other and the
// split is permanent — the one divergence anti-entropy cannot repair.
//
// The check used to report it as convergence, because the fingerprint folded
// the version and not the value. Measured on a six-node cluster before the
// fix: 45 seconds of anti-entropy, both sides still holding different values,
// `converged: true`, every peer agreeing, and `/diagnostics` saying the node
// looked healthy.
//
// The twins run in a cluster of their own, and the split is seeded into their
// state files rather than raced for, because that is the state a copied file
// plus one write on each side reaches and seeding it makes this deterministic.
func TestTwinsUnderOneIdentityAreReportedNotHidden(t *testing.T) {
	const key = "split-brain"

	// Both really do hold different values for the one key.
	va, _, oka := twinA.get(t, key)
	vb, _, okb := twinB.get(t, key)
	if !oka || !okb {
		t.Fatalf("the twins do not both hold %s: a=%v b=%v", key, oka, okb)
	}
	if va == vb {
		t.Fatalf("the twins agree on %s (%q); the split was not seeded", key, va)
	}

	var found peerConsistency
	eventually(t, "the twins to see each other and report the split", converge, func() (bool, string) {
		r := twinA.consistency(t)
		if len(r.Peers) == 0 {
			return false, "twin-a has not met twin-b yet"
		}
		if r.Converged {
			return false, "the report still claims the twins agree"
		}
		for _, p := range r.Peers {
			if p.Agrees || !p.Reachable {
				continue
			}
			found = p
			return true, ""
		}
		return false, "no peer is reported as disagreeing"
	})

	var named *keyDifference
	for i := range found.Differences {
		if found.Differences[i].Key == key {
			named = &found.Differences[i]
		}
	}
	if named == nil {
		t.Fatalf("the report differs in %d bucket(s) but does not name %s: %+v",
			len(found.DifferingBuckets), key, found.Differences)
	}
	// The whole point: both sides at the identical version. Reporting it as an
	// ordinary difference would suggest anti-entropy will sort it out.
	if !named.SameVersion {
		t.Errorf("%s: the difference is not marked as one replication cannot repair: %+v", key, named)
	}
	if named.Local != named.Remote {
		t.Errorf("%s: versions reported as %q and %q, want the identical version on both sides",
			key, named.Local, named.Remote)
	}

	// And the cause is named, not just the symptom.
	eventually(t, "the identity conflict to be reported", converge, func() (bool, string) {
		for _, c := range twinA.diagnostics(t).Checks {
			if strings.Contains(c.Title, "using this node's identity") {
				if c.Severity != "error" {
					return false, "the identity conflict is reported as " + c.Severity
				}
				if !strings.Contains(c.Detail, "state file") {
					return false, "the check does not say how to fix it"
				}
				return true, ""
			}
		}
		return false, "no check names the node sharing this node's identity"
	})
}
