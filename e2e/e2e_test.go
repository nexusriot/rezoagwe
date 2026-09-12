// Package e2e drives a real rezoagwe cluster — separate processes, real UDP and
// TCP, a real rendezvous — over its HTTP gateway.
//
// What it is for: the properties that only exist between nodes. A store merges
// correctly in a unit test and still fails to converge because the update never
// left the process, the key it was framed with differed by a character, or the
// snapshot a joiner pulled was appended to history it already had. None of that
// is visible from inside one process, and all of it has been wrong here before.
//
// Tests run in the order they are written. Later ones depend on the cluster the
// earlier ones settled — that is deliberate, because the interesting failures
// are cumulative, and it is what a real deployment looks like.
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	// Generous next to a healthy cluster (a write lands in well under a second)
	// and still far below the point where a stuck suite wastes anyone's time.
	converge = 30 * time.Second
	// Membership goes through the rendezvous and a gossip round, so it is the
	// slowest thing here.
	settle = 60 * time.Second
)

// Written before the late joiner exists, so what it ends up holding can only
// have come from a sync.
const (
	preJoinKey  = "pre-join/secret-of-the-mountain"
	preJoinVal  = "written before dave arrived"
	preJoinChat = "this was said before dave arrived"
)

func TestMain(m *testing.M) {
	// Seed before anything else runs. The point is to get this in while the late
	// joiner is still asleep, so that what it ends up holding can only have
	// arrived by syncing — which means it cannot wait for a test to do it.
	if err := seed(); err != nil {
		fmt.Fprintln(os.Stderr, "seeding the cluster failed:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// seed writes the pre-join fixtures with no *testing.T in hand, so it reports
// by error rather than by failing a test that has not started yet.
func seed() error {
	deadline := time.Now().Add(settle)
	for {
		err := post("PUT", a.url+"/kv/"+preJoinKey, preJoinVal)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("seeding %s: %w", preJoinKey, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err := post("POST", a.url+"/chat", preJoinChat); err != nil {
		return fmt.Errorf("seeding the chat log: %w", err)
	}
	return nil
}

func post(method, url, body string) error {
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("%s %s = %s", method, url, resp.Status)
	}
	return nil
}

// ---- membership ------------------------------------------------------------

// Nothing is told about anyone: every node is given one rendezvous address and
// has to find the rest for itself.
func TestClusterFormsThroughTheRendezvous(t *testing.T) {
	want := []string{"node-a:3137", "node-b:3137", "node-c:3137", "node-leaver:3137"}
	for _, n := range core {
		n := n
		eventually(t, n.name+" to learn the other nodes", settle, func() (bool, string) {
			got := n.peerAddrs(t)
			for _, w := range want {
				if w == n.name+":3137" {
					continue
				}
				if !contains(got, w) {
					return false, fmt.Sprintf("%s knows %v, missing %s", n.name, got, w)
				}
			}
			return true, ""
		})
	}

	// A node never gossips itself into its own peer list, however it was
	// addressed on the command line.
	for _, n := range core {
		if contains(n.peerAddrs(t), n.name+":3137") {
			t.Errorf("%s lists itself as a peer: %v", n.name, n.peerAddrs(t))
		}
	}

	if h := a.health(t); !h.Connected || h.Cluster != "e2e" {
		t.Errorf("node-a health = %+v, want connected in cluster e2e", h)
	}
}

// ---- replication -----------------------------------------------------------

// The write path proper: a value handed to one node, read back from the two it
// was not handed to, with the same version everywhere.
func TestAWriteReachesEveryNode(t *testing.T) {
	version := a.put(t, "colour", "indigo")
	valueEverywhere(t, core, "colour", "indigo", converge)

	for _, n := range core {
		_, got, _ := n.get(t, "colour")
		if got != version {
			t.Errorf("%s has version %q, node-a wrote %q", n.name, got, version)
		}
	}
	if !strings.HasSuffix(version, ".") && !strings.Contains(version, ".") {
		t.Errorf("version %q is not the counter.node form", version)
	}
}

// Two writers, one key, no coordination. Which value wins is the store's
// business; that every node agrees on the same one is the cluster's.
func TestConcurrentWritesConvergeOnOneValue(t *testing.T) {
	done := make(chan struct{}, 2)
	go func() { a.put(t, "contested", "from-alice"); done <- struct{}{} }()
	go func() { b.put(t, "contested", "from-bob"); done <- struct{}{} }()
	<-done
	<-done

	eventually(t, "every node to agree on one winner", converge, func() (bool, string) {
		var value, version string
		for i, n := range core {
			v, ver, ok := n.get(t, "contested")
			if !ok {
				return false, n.name + " has no value yet"
			}
			if i == 0 {
				value, version = v, ver
				continue
			}
			if v != value || ver != version {
				return false, fmt.Sprintf("%s has %q@%s, node-a has %q@%s", n.name, v, ver, value, version)
			}
		}
		return true, ""
	})

	value, _, _ := a.get(t, "contested")
	if value != "from-alice" && value != "from-bob" {
		t.Errorf("converged on %q, which neither node wrote", value)
	}
}

// ---- compare-and-swap ------------------------------------------------------

// If-Match is the lock this store offers. It has to hold across nodes, not just
// against the copy the caller is talking to.
func TestCompareAndSwapRefusesAStaleVersion(t *testing.T) {
	version := a.put(t, "counter", "1")
	valueEverywhere(t, core, "counter", "1", converge)

	// b writes with the version it can see: allowed.
	_, seen, _ := b.get(t, "counter")
	if code, _ := b.putIfMatch(t, "counter", "2", seen); code != 204 {
		t.Fatalf("guarded write with the current version = %d, want 204", code)
	}
	valueEverywhere(t, core, "counter", "2", converge)

	// c writes with the version it saw before b moved it: refused.
	if code, _ := c.putIfMatch(t, "counter", "3", version); code != 412 {
		t.Errorf("guarded write with a stale version = %d, want 412", code)
	}

	// And refused everywhere, not just locally: a rejected CAS must not have
	// been broadcast to anyone.
	time.Sleep(2 * time.Second)
	for _, n := range core {
		if v, _, _ := n.get(t, "counter"); v != "2" {
			t.Errorf("%s has %q after a refused guarded write, want 2", n.name, v)
		}
	}
}

// If-Match: * is the must-not-exist form — the primitive a lock is built on, so
// exactly one of two racing creators has to lose.
func TestCompareAndSwapCreatesOnlyOnce(t *testing.T) {
	if code, _ := a.putIfMatch(t, "lock/leader", "alice", "*"); code != 204 {
		t.Fatalf("creating a fresh key with If-Match:* = %d, want 204", code)
	}
	valueEverywhere(t, core, "lock/leader", "alice", converge)

	if code, _ := b.putIfMatch(t, "lock/leader", "bob", "*"); code != 412 {
		t.Errorf("claiming a key that already exists = %d, want 412", code)
	}
	if v, _, _ := a.get(t, "lock/leader"); v != "alice" {
		t.Errorf("the loser overwrote the winner: %q", v)
	}
}

// ---- expiry and deletion ---------------------------------------------------

// Expiry is computed, not announced: each replica has to drop the key on its
// own and still agree with the others.
func TestATtlExpiresOnEveryNode(t *testing.T) {
	b.putTTL(t, "ephemeral", "here-for-now", 5)
	valueEverywhere(t, core, "ephemeral", "here-for-now", converge)

	before := a.health(t).Tombstones
	absentEverywhere(t, core, "ephemeral", converge)

	eventually(t, "the expiry to leave a tombstone", converge, func() (bool, string) {
		if got := a.health(t).Tombstones; got <= before {
			return false, fmt.Sprintf("tombstones still %d", got)
		}
		return true, ""
	})
}

// A delete is a versioned tombstone, so it has to travel like a write and it has
// to outrank the value it replaced.
func TestADeleteReplicatesAsATombstone(t *testing.T) {
	a.put(t, "doomed", "still-here")
	valueEverywhere(t, core, "doomed", "still-here", converge)

	c.del(t, "doomed")
	absentEverywhere(t, core, "doomed", converge)

	// The tombstone is in the history, and it is the newest thing there. The
	// history reads oldest first, so the delete is the last entry.
	h := a.history(t, "doomed")
	if len(h) < 2 {
		t.Fatalf("history for the deleted key has %d entries, want the write and the delete", len(h))
	}
	if h[0].Deleted || h[0].Value != "still-here" {
		t.Errorf("oldest history entry is %+v, want the original write", h[0])
	}
	if newest := h[len(h)-1]; !newest.Deleted {
		t.Errorf("newest history entry is %+v, want the delete", newest)
	}
}

func TestHistoryRecordsEveryVersion(t *testing.T) {
	a.put(t, "diary", "monday")
	valueEverywhere(t, core, "diary", "monday", converge)
	b.put(t, "diary", "tuesday")
	valueEverywhere(t, core, "diary", "tuesday", converge)

	// c saw both writes and neither was its own, so it is the interesting one.
	eventually(t, "node-c to record both versions", converge, func() (bool, string) {
		h := c.history(t, "diary")
		if len(h) < 2 {
			return false, fmt.Sprintf("history has %d entries", len(h))
		}
		return true, ""
	})
	h := c.history(t, "diary")
	if h[0].Value != "monday" {
		t.Errorf("oldest version is %q, want monday", h[0].Value)
	}
	if newest := h[len(h)-1]; newest.Value != "tuesday" {
		t.Errorf("newest version is %q, want tuesday", newest.Value)
	}
	// Chronological, which is the only order in which a history is worth
	// reading, and the one a client can rely on.
	for i := 1; i < len(h); i++ {
		if h[i].At < h[i-1].At {
			t.Errorf("history is out of order at %d: %+v before %+v", i, h[i-1], h[i])
		}
	}
	for _, e := range h {
		if e.Local {
			t.Errorf("node-c marked %+v as a local write; it wrote neither", e)
		}
	}
}

// ---- joining ---------------------------------------------------------------

// The joiner starts empty, long after the writes it is missing. Anti-entropy
// and state sync are the only routes to the data, and the only way to know they
// ran is to ask a node that could not have been told any other way.
func TestALateJoinerConvergesOnTheWholeStore(t *testing.T) {
	waitReady(t, late, envSeconds("E2E_LATE_JOIN_SEC", 25)+settle)

	valueEverywhere(t, []node{late}, preJoinKey, preJoinVal, settle)

	// Not just the one key: the whole shape of the store.
	eventually(t, "the late joiner to hold what node-a holds", settle, func() (bool, string) {
		want, got := keySet(a.entries(t)), keySet(late.entries(t))
		for k := range want {
			if _, ok := got[k]; !ok {
				return false, fmt.Sprintf("missing %q (has %d of %d keys)", k, len(got), len(want))
			}
		}
		return true, ""
	})

	// And it is a peer of everyone, not just of whoever it synced from.
	for _, n := range core {
		n := n
		eventually(t, n.name+" to know the late joiner", settle, func() (bool, string) {
			if !contains(n.peerAddrs(t), "node-late:3137") {
				return false, fmt.Sprintf("%s knows %v", n.name, n.peerAddrs(t))
			}
			return true, ""
		})
	}
}

func keySet(entries []entry) map[string]string {
	out := map[string]string{}
	for _, e := range entries {
		out[e.Key] = e.Value
	}
	return out
}

// A node that comes back is not a node that arrives: it already holds most of
// what it is about to be handed. Restarting is also the one moment it is
// guaranteed to ask a peer for a snapshot, which makes it the case where merging
// that snapshot into history it already has either works or shows the whole
// conversation twice.
func TestARestartedNodeKeepsItsStoreWithoutRelivingTheConversation(t *testing.T) {
	// Not "is it answering" — it was answering before it went down. The uptime
	// going backwards is what says the process behind the address is a new one.
	waitRestarted(t, restart, envSeconds("E2E_RESTART_SEC", 30)+settle)
	eventually(t, "the restarted node to rejoin", settle, func() (bool, string) {
		if h := restart.health(t); !h.Connected {
			return false, fmt.Sprintf("peers = %d", h.Peers)
		}
		return true, ""
	})

	// The store came back with it rather than being fetched from scratch — and
	// either way it has to agree with everyone else.
	valueEverywhere(t, []node{restart}, preJoinKey, preJoinVal, settle)

	eventually(t, "the restarted node to catch up on the chat", settle, func() (bool, string) {
		if countText(restart.chat(t), preJoinChat) == 0 {
			return false, "it has not received the pre-join line yet"
		}
		return true, ""
	})
	// Let any further snapshot land before counting: the failure this is looking
	// for adds copies, so waiting can only make it more visible.
	time.Sleep(5 * time.Second)

	if got := countText(restart.chat(t), preJoinChat); got != 1 {
		t.Errorf("the restarted node holds the pre-join line %d times, want 1", got)
	}
	assertNoDuplicateChat(t, restart)
}

func assertNoDuplicateChat(t *testing.T, n node) {
	t.Helper()
	seen := map[string]int{}
	for _, e := range n.chat(t) {
		key := fmt.Sprintf("%d\x00%s\x00%s\x00%s", e.TS, e.Sender, e.Kind, e.Text)
		seen[key]++
		if seen[key] == 2 {
			t.Errorf("%s holds %q more than once", n.name, e.Text)
		}
	}
}

// ---- chat ------------------------------------------------------------------

func TestChatReachesEveryNode(t *testing.T) {
	b.say(t, "anyone seen the ferry")
	eventually(t, "the message to reach every node", converge, func() (bool, string) {
		for _, n := range cluster {
			if countText(n.chat(t), "anyone seen the ferry") == 0 {
				return false, n.name + " has not received it"
			}
		}
		return true, ""
	})

	// Attribution survives the trip: a message is from a nick, not from
	// whichever socket happened to deliver it.
	for _, e := range a.chat(t) {
		if e.Text == "anyone seen the ferry" && e.Nick != "bob" {
			t.Errorf("message attributed to %q, want bob", e.Nick)
		}
	}
}

// The slash commands are part of the protocol, not of the terminal client: the
// gateway feeds them to the same parser.
func TestEmotesArriveAsActions(t *testing.T) {
	c.say(t, "/me waves from the quay")
	eventually(t, "the emote to arrive as an action", converge, func() (bool, string) {
		for _, e := range a.chat(t) {
			if e.Text == "waves from the quay" {
				if e.Kind != "action" {
					return false, fmt.Sprintf("kind is %q, want action", e.Kind)
				}
				return true, ""
			}
		}
		return false, "node-a has not received it"
	})
}

// A direct message goes to one address. The interesting part is not that it
// arrives — it is that it stays out of everything that copies chat around: the
// third node never sees it, and neither does a node that syncs history later.
func TestDirectMessagesStayBetweenTheTwoEnds(t *testing.T) {
	const secret = "meet me at the old pier"
	a.say(t, "/msg bob "+secret)

	eventually(t, "the direct message to reach bob", converge, func() (bool, string) {
		if countText(b.chat(t), secret) == 0 {
			return false, "node-b has not received it"
		}
		return true, ""
	})

	// Give a gossip round the chance to leak it before declaring it private.
	time.Sleep(3 * time.Second)
	for _, n := range []node{c, late} {
		if countText(n.chat(t), secret) != 0 {
			t.Errorf("%s received a direct message it was not part of", n.name)
		}
	}
}

// A snapshot repeats history the receiver may already hold. Merging it by
// appending showed the same conversation once per sync — invisible on a two
// node cluster that only ever syncs once, obvious to anyone who joins late.
func TestSyncedHistoryIsNotDuplicated(t *testing.T) {
	for _, n := range cluster {
		assertNoDuplicateChat(t, n)
	}

	// The joiner in particular: it received the whole log as a snapshot, and
	// that snapshot overlapped the lines it had already heard live.
	if got := countText(late.chat(t), preJoinChat); got != 1 {
		t.Errorf("the late joiner holds the pre-join line %d times, want 1: %v",
			got, chatTexts(late.chat(t)))
	}
}

// ---- export and import -----------------------------------------------------

// The escape hatch: what comes out has to be enough to put back, and putting it
// back has to obey the same version rules as any other write.
func TestExportAndImportRoundTrip(t *testing.T) {
	a.put(t, "archive/one", "first")
	valueEverywhere(t, core, "archive/one", "first", converge)

	var dump struct {
		Node    string `json:"node"`
		Cluster string `json:"cluster"`
		Entries []struct {
			Key     string `json:"key"`
			Value   string `json:"value"`
			Action  string `json:"action"`
			Version struct {
				Counter uint64 `json:"counter"`
				Node    string `json:"node"`
			} `json:"version"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(a.export(t)), &dump); err != nil {
		t.Fatalf("the export is not valid JSON: %v", err)
	}
	if dump.Cluster != "e2e" {
		t.Errorf("export says cluster %q, want e2e", dump.Cluster)
	}
	found := false
	for _, e := range dump.Entries {
		if e.Key == "archive/one" && e.Value == "first" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the export is missing archive/one: %+v", dump.Entries)
	}

	// An import older than what the cluster holds must be ignored rather than
	// resurrect the past — that is last-write-wins doing its job on a restore.
	stale := `{"node":"restore","cluster":"e2e","entries":[{"action":"set","key":"archive/one",` +
		`"value":"a much older first","version":{"counter":1,"node":"ancient"}}]}`
	c.importState(t, stale)
	time.Sleep(2 * time.Second)
	for _, n := range core {
		if v, _, _ := n.get(t, "archive/one"); v != "first" {
			t.Errorf("%s took a stale import: %q", n.name, v)
		}
	}

	// A seeding import is the deliberate override, and it replicates.
	code, _, body := c.do(t, "POST", "/import?mode=seed", stale, nil)
	if code != 200 {
		t.Fatalf("seeding import = %d: %s", code, body)
	}
	valueEverywhere(t, core, "archive/one", "a much older first", converge)
}

// ---- isolation -------------------------------------------------------------

// Both strangers know the rendezvous address and share the network. The key and
// the cluster name are the whole of the difference, and the whole of the
// defence: every packet is framed with a key derived from them.
func TestAStrangerWithTheWrongKeyNeverJoins(t *testing.T) {
	assertShutOut(t, wrongPS, "stranger-key:3137")
}

func TestAStrangerInAnotherClusterNeverJoins(t *testing.T) {
	assertShutOut(t, wrongCl, "stranger-cluster:3137")
}

func assertShutOut(t *testing.T, stranger node, addr string) {
	t.Helper()
	waitReady(t, stranger, settle)

	// It has been running as long as the cluster has, so this is not a matter
	// of not having tried yet.
	if h := stranger.health(t); h.Peers != 0 || h.Keys != 0 {
		t.Errorf("%s got in: %+v", stranger.name, h)
	}
	for _, n := range cluster {
		if contains(n.peerAddrs(t), addr) {
			t.Errorf("%s accepted %s as a peer: %v", n.name, addr, n.peerAddrs(t))
		}
	}
	if v, _, ok := stranger.get(t, preJoinKey); ok {
		t.Errorf("%s read cluster data: %q", stranger.name, v)
	}
}

// ---- leaving ---------------------------------------------------------------

// A node that shuts down cleanly says so. Without the goodbye the cluster still
// converges — the eviction timer gets there eventually — which is exactly why
// this is worth asserting rather than assuming: the difference between the two
// is fifteen seconds of a peer list that is wrong, and nothing else.
func TestALeavingNodeAnnouncesItself(t *testing.T) {
	leaveAfter := envSeconds("E2E_LEAVE_SEC", 45)

	eventually(t, "the leaver to shut down", leaveAfter+settle, func() (bool, string) {
		if contains(a.peerAddrs(t), "node-leaver:3137") {
			return false, "node-a still lists it"
		}
		return true, ""
	})

	// The goodbye is what did it, not the timer: the counter separates them.
	if got := a.metric(t, `rezoagwe_messages_total{kind="goodbye",direction="received"}`); got < 1 {
		t.Errorf("node-a received %v goodbyes; the leaver went quiet instead of announcing", got)
	}

	// Every node drops it, not just the one that happened to be watching.
	for _, n := range []node{b, c, late} {
		n := n
		eventually(t, n.name+" to drop the leaver", settle, func() (bool, string) {
			if contains(n.peerAddrs(t), "node-leaver:3137") {
				return false, fmt.Sprintf("%s still lists it", n.name)
			}
			return true, ""
		})
	}

	// And the departure is in the log the way an operator would look for it.
	found := false
	for _, e := range a.chat(t) {
		if strings.Contains(e.Text, "left") && strings.Contains(e.Text, "node-leaver:3137") {
			found = true
		}
	}
	if !found {
		t.Errorf("no 'left' line for the leaver in node-a's log: %v", chatTexts(a.chat(t)))
	}
}

// ---- instrumentation -------------------------------------------------------

// The counters are how anyone diagnoses this thing in the field, so they have to
// mean what they say after a run that exercised all of it.
func TestTheCountersReflectWhatHappened(t *testing.T) {
	for _, n := range core {
		if got := n.metric(t, "rezoagwe_packets_sent_total"); got == 0 {
			t.Errorf("%s sent no packets", n.name)
		}
		if got := n.metric(t, "rezoagwe_kv_applied_total"); got == 0 {
			t.Errorf("%s applied no remote updates, so nothing replicated to it", n.name)
		}
		// Every frame is authenticated; a run with no bad key in it should have
		// nothing to reject, and the strangers never got far enough to try.
		if got := n.metric(t, "rezoagwe_auth_failures_total"); got != 0 {
			t.Errorf("%s dropped %v packets on the key check", n.name, got)
		}
		if got := n.metric(t, "rezoagwe_malformed_drops_total"); got != 0 {
			t.Errorf("%s could not parse %v packets it had authenticated", n.name, got)
		}
		if got := n.metric(t, "rezoagwe_send_errors_total"); got != 0 {
			t.Errorf("%s failed to send %v datagrams", n.name, got)
		}
	}

	// The joiner is the one that had to be repaired, so it is where the sync
	// counters should be non-zero.
	if in := late.metric(t, "rezoagwe_state_sync_in_total"); in == 0 {
		t.Errorf("the late joiner received no snapshot; it should not have had the store")
	}
}
