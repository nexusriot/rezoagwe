package e2e

import (
	"net/http"
	"strings"
	"testing"
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
