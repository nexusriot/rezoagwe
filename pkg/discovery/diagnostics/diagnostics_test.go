package diagnostics

import (
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/rezoagwe/pkg/discovery/model"
	"github.com/nexusriot/rezoagwe/pkg/discovery/topology"
	"github.com/nexusriot/rezoagwe/pkg/metrics"
)

var now = time.Unix(10_000, 0)

// healthy is a node with nothing wrong with it, which every test starts from
// and breaks in exactly one way.
func healthy() Snapshot {
	return Snapshot{
		Addr:              "10.0.0.1:3137",
		BindAddr:          "10.0.0.1:3137",
		LocalIPv4:         "10.0.0.1",
		Nick:              "alice",
		Cluster:           "rezoagwe",
		KeyFingerprint:    "deadbeef",
		PSKSet:            true,
		UptimeSec:         600,
		StreamListener:    true,
		Seeds:             []string{"10.0.0.9:9999"},
		Peers:             []Peer{{Addr: "10.0.0.2:3137", Nick: "bob", LastSeen: now.Unix() - 2, PeerCount: metrics.PeerCount{PacketsIn: 10, PacketsOut: 10}}},
		Strangers:         []Stranger{},
		Gauges:            metrics.Gauges{Peers: 1, Keys: 3},
		HeartbeatInterval: 5 * time.Second,
		EvictThreshold:    15 * time.Second,
		Topology: topology.Build(topology.Input{
			SelfAddr: "10.0.0.1:3137",
			Peers:    []topology.Peer{{Addr: "10.0.0.2:3137"}},
			Now:      now,
		}),
	}
}

func titles(checks []Check) string {
	var b strings.Builder
	for _, c := range checks {
		b.WriteString(string(c.Severity) + ":" + c.Title + "\n")
	}
	return b.String()
}

func findCheck(t *testing.T, checks []Check, substr string) Check {
	t.Helper()
	for _, c := range checks {
		if strings.Contains(c.Title, substr) {
			return c
		}
	}
	t.Fatalf("no check mentioning %q in:\n%s", substr, titles(checks))
	return Check{}
}

func mustNotFind(t *testing.T, checks []Check, substr string) {
	t.Helper()
	for _, c := range checks {
		if strings.Contains(c.Title, substr) {
			t.Fatalf("unexpected check %q in:\n%s", c.Title, titles(checks))
		}
	}
}

func TestAHealthyNodeSaysSo(t *testing.T) {
	checks := Checks(healthy(), now)
	if len(checks) == 0 || checks[0].Severity != SeverityOK {
		t.Fatalf("healthy node did not lead with an OK:\n%s", titles(checks))
	}
	if Worst(checks) != SeverityOK {
		t.Fatalf("worst severity = %q, want ok:\n%s", Worst(checks), titles(checks))
	}
}

// The claim: auth failures mean a key or cluster-name mismatch, and the finding
// has to name the fingerprint that lets two operators compare without sharing a
// secret.
func TestAuthFailuresBlameTheKey(t *testing.T) {
	d := healthy()
	d.Metrics.AuthFailures = 3
	c := findCheck(t, Checks(d, now), "failed authentication")
	if c.Severity != SeverityError {
		t.Fatalf("severity = %q, want error", c.Severity)
	}
	if !strings.Contains(c.Detail, "deadbeef") || !strings.Contains(c.Detail, "rezoagwe") {
		t.Fatalf("detail names neither the fingerprint nor the cluster: %s", c.Detail)
	}
}

func TestSkewDropsBlameAClock(t *testing.T) {
	d := healthy()
	d.Metrics.SkewDrops = 1
	c := findCheck(t, Checks(d, now), "clock skew")
	if c.Severity != SeverityError || !strings.Contains(c.Detail, "clock") {
		t.Fatalf("check = %+v", c)
	}
}

// A peer this node talks to and never hears from is a firewall, not loss — and
// the rule must say so rather than leaving it as a packet count.
func TestOneWayTrafficIsReportedAsAFirewall(t *testing.T) {
	d := healthy()
	d.Peers = []Peer{{Addr: "10.0.0.2:3137", PeerCount: metrics.PeerCount{PacketsOut: 40, PacketsIn: 0}}}
	c := findCheck(t, Checks(d, now), "never answered")
	if c.Severity != SeverityError || !strings.Contains(c.Detail, "firewall") {
		t.Fatalf("check = %+v", c)
	}
}

// The same peer during the first seconds of a start is not yet a finding.
func TestOneWayTrafficIsNotReportedBeforeTheNodeSettles(t *testing.T) {
	d := healthy()
	d.UptimeSec = 1
	d.Peers = []Peer{{Addr: "10.0.0.2:3137", PeerCount: metrics.PeerCount{PacketsOut: 2}}}
	mustNotFind(t, Checks(d, now), "never answered")
}

func TestNoPeersAndNoSeedsIsExplained(t *testing.T) {
	d := healthy()
	d.Peers = nil
	d.Seeds = nil
	d.Topology = topology.Build(topology.Input{SelfAddr: d.Addr, Now: now})
	c := findCheck(t, Checks(d, now), "No peers and no seeds")
	if c.Severity != SeverityWarn {
		t.Fatalf("severity = %q", c.Severity)
	}
}

func TestNoPeersDespiteSeedsNamesTheSeeds(t *testing.T) {
	d := healthy()
	d.Peers = nil
	d.Topology = topology.Build(topology.Input{SelfAddr: d.Addr, Now: now})
	c := findCheck(t, Checks(d, now), "No peers learned")
	if !strings.Contains(c.Detail, "10.0.0.9:9999") {
		t.Fatalf("detail does not name the seed: %s", c.Detail)
	}
}

// The trap this whole feature exists for: a node bound to ":3137" tells every
// peer to reach it at an address that resolves to the peer's own machine.
func TestLoopbackAdvertiseIsFlagged(t *testing.T) {
	d := healthy()
	d.Addr = ":3137"
	c := findCheck(t, Checks(d, now), "loopback")
	if c.Severity != SeverityWarn || !strings.Contains(c.Detail, "-advertise") {
		t.Fatalf("check does not point at the fix: %+v", c)
	}
}

func TestAnOverriddenAdvertiseAddressIsInformational(t *testing.T) {
	d := healthy()
	d.Addr = "203.0.113.7:3137"
	d.AdvertiseAddr = "203.0.113.7:3137"
	c := findCheck(t, Checks(d, now), "Advertise address is overridden")
	if c.Severity != SeverityInfo {
		t.Fatalf("severity = %q, want info", c.Severity)
	}
}

func TestStrangersAreReportedAsANatSignature(t *testing.T) {
	d := healthy()
	d.Strangers = []Stranger{{Addr: "192.168.1.5:51234", PacketsIn: 4}}
	c := findCheck(t, Checks(d, now), "without being a peer")
	if !strings.Contains(c.Detail, "NAT") {
		t.Fatalf("detail = %s", c.Detail)
	}
}

func TestStalePeersAreWarnedBeforeEviction(t *testing.T) {
	d := healthy()
	d.Peers = []Peer{{Addr: "10.0.0.2:3137", Nick: "bob", LastSeen: now.Unix() - 12,
		PeerCount: metrics.PeerCount{PacketsIn: 1, PacketsOut: 1}}}
	c := findCheck(t, Checks(d, now), "going stale")
	if !strings.Contains(c.Detail, "bob") {
		t.Fatalf("detail does not name the peer: %s", c.Detail)
	}
}

// A partition is the failure a KV store cannot report on its own, so this is
// the check that most has to work.
func TestAPartitionIsReported(t *testing.T) {
	d := healthy()
	d.Topology = topology.Build(topology.Input{
		SelfAddr: "10.0.0.1:3137",
		Peers:    []topology.Peer{{Addr: "10.0.0.2:3137"}},
		Views: map[string]model.PeerView{
			"10.0.0.8:3137": {Peers: []string{"10.0.0.9:3137"}, At: now},
			"10.0.0.9:3137": {Peers: []string{"10.0.0.8:3137"}, At: now},
		},
		Now: now,
	})
	c := findCheck(t, Checks(d, now), "partitioned")
	if c.Severity != SeverityError || !strings.Contains(c.Detail, "2 | 2") {
		t.Fatalf("check = %+v", c)
	}
}

func TestNoPskIsInformationalNotAWarning(t *testing.T) {
	d := healthy()
	d.PSKSet = false
	c := findCheck(t, Checks(d, now), "No pre-shared key")
	if c.Severity != SeverityInfo {
		t.Fatalf("severity = %q, want info", c.Severity)
	}
	// It must not suppress the healthy line: an info is not a problem.
	if Checks(d, now)[0].Severity != SeverityOK {
		t.Fatal("an info finding suppressed the healthy line")
	}
}

func TestTombstonesKeptForeverAreMentionedOnlyWhenThereAreSome(t *testing.T) {
	d := healthy()
	mustNotFind(t, Checks(d, now), "tombstone")
	d.Gauges.Tombstones = 5
	findCheck(t, Checks(d, now), "tombstone")
}

func TestSendErrorNamesTheLastFailure(t *testing.T) {
	d := healthy()
	d.Metrics.SendErrors = 2
	d.Metrics.LastSendError = &metrics.SendError{Addr: "10.0.0.4:3137", Kind: "hello", Message: "no route to host"}
	c := findCheck(t, Checks(d, now), "send error")
	if !strings.Contains(c.Detail, "10.0.0.4:3137") || !strings.Contains(c.Detail, "no route to host") {
		t.Fatalf("detail = %s", c.Detail)
	}
}

func TestAnyWarningSuppressesTheHealthyLine(t *testing.T) {
	d := healthy()
	d.StreamListener = false
	checks := Checks(d, now)
	if checks[0].Severity == SeverityOK {
		t.Fatalf("healthy line survived a warning:\n%s", titles(checks))
	}
	if Worst(checks) != SeverityWarn {
		t.Fatalf("worst = %q, want warn", Worst(checks))
	}
}

func TestReportIsPasteableAndCoversTheFindings(t *testing.T) {
	d := healthy()
	d.Metrics.AuthFailures = 1
	out := Report(d, now)
	for _, want := range []string{"rezoagwe node diagnostics", "10.0.0.1:3137", "deadbeef",
		"peers (1)", "topology:", "checks", "[ERROR]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report missing %q:\n%s", want, out)
		}
	}
}

// A node with no -advertise is advertising the address it bound, not nothing.
func TestReportNamesTheAdvertisedAddressEvenWhenItIsTheBindAddress(t *testing.T) {
	d := healthy()
	d.AdvertiseAddr = ""
	out := Report(d, now)
	if !strings.Contains(out, "advertising 10.0.0.1:3137 (from -node)") {
		t.Fatalf("report does not say what is advertised:\n%s", out)
	}

	d.AdvertiseAddr = "203.0.113.7:3137"
	out = Report(d, now)
	if !strings.Contains(out, "advertising 203.0.113.7:3137 (-advertise)") {
		t.Fatalf("report does not mark an overridden advertise address:\n%s", out)
	}
}
