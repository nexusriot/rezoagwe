package metrics

import (
	"errors"
	"strings"
	"testing"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

func TestPerPeerCountersSeparateTheDirections(t *testing.T) {
	m := New()
	m.SentTo("10.0.0.2:3137", pb.KindHello, 60)
	m.SentTo("10.0.0.2:3137", pb.KindKV, 100)
	m.RecvFrom("10.0.0.2:3137", pb.KindHello, 60)
	m.SentTo("10.0.0.3:3137", pb.KindHello, 60)

	two, ok := m.PeerCounters("10.0.0.2:3137")
	if !ok {
		t.Fatal("no counters for a peer that was written to")
	}
	if two.PacketsOut != 2 || two.BytesOut != 160 || two.PacketsIn != 1 || two.BytesIn != 60 {
		t.Fatalf("peer counters = %+v", two)
	}

	// The pair that separates "quiet" from "unreachable".
	three, _ := m.PeerCounters("10.0.0.3:3137")
	if three.PacketsOut != 1 || three.PacketsIn != 0 {
		t.Fatalf("one-way peer = %+v", three)
	}

	// The global counters still add up across peers.
	s := m.Snapshot()
	if s.PacketsSent != 3 || s.PacketsRecv != 1 {
		t.Fatalf("global counters = sent %d recv %d", s.PacketsSent, s.PacketsRecv)
	}
	if len(s.Peers) != 2 || s.Peers[0].Addr != "10.0.0.2:3137" {
		t.Fatalf("snapshot peers = %+v", s.Peers)
	}
}

// "12 send errors" is not actionable; the address that stopped routing is.
func TestSendFailedKeepsTheLastErrorWhole(t *testing.T) {
	m := New()
	m.SendFailed("10.0.0.4:3137", pb.KindKV, errors.New("no route to host"), 1234)

	s := m.Snapshot()
	if s.SendErrors != 1 {
		t.Fatalf("SendErrors = %d", s.SendErrors)
	}
	e := s.LastSendError
	if e == nil || e.Addr != "10.0.0.4:3137" || e.Kind != "kv" ||
		!strings.Contains(e.Message, "no route") || e.At != 1234 {
		t.Fatalf("last error = %+v", e)
	}
	if c, _ := m.PeerCounters("10.0.0.4:3137"); c.SendErrors != 1 {
		t.Fatal("the failure was not attributed to the peer")
	}
}

func TestRejectedFromAttributesAuthFailuresToASource(t *testing.T) {
	m := New()
	m.RejectedFrom("192.168.1.9:40000")
	m.RejectedFrom("192.168.1.9:40000")
	if c, _ := m.PeerCounters("192.168.1.9:40000"); c.Rejected != 2 {
		t.Fatalf("rejected = %d, want 2", c.Rejected)
	}
}

func TestAnEmptyAddressIsNotTracked(t *testing.T) {
	m := New()
	m.SentTo("", pb.KindHello, 10)
	if len(m.Snapshot().Peers) != 0 {
		t.Fatal("an empty address created a peer entry")
	}
}

func TestGaugesComeFromTheRegisteredSource(t *testing.T) {
	m := New()
	if got := m.Snapshot().Gauges; got != (Gauges{}) {
		t.Fatalf("gauges without a source = %+v, want zero", got)
	}
	m.SetGauges(func() Gauges { return Gauges{Peers: 2, Keys: 7, ValueBytes: 42} })

	s := m.Snapshot()
	if s.Gauges.Keys != 7 || s.Gauges.Peers != 2 || s.Gauges.ValueBytes != 42 {
		t.Fatalf("gauges = %+v", s.Gauges)
	}
}

// Levels are what an alert is written against, so they have to be typed as
// gauges rather than smuggled in as counters.
func TestPrometheusDeclaresGaugesAsGauges(t *testing.T) {
	m := New()
	m.SetGauges(func() Gauges { return Gauges{Peers: 3, Keys: 9, Tombstones: 1, Clock: 55} })
	out := m.Snapshot().Prometheus()

	for _, want := range []string{
		"# TYPE rezoagwe_peers gauge\nrezoagwe_peers 3",
		"# TYPE rezoagwe_keys gauge\nrezoagwe_keys 9",
		"# TYPE rezoagwe_tombstones gauge\nrezoagwe_tombstones 1",
		"# TYPE rezoagwe_lamport_clock gauge\nrezoagwe_lamport_clock 55",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition missing:\n%s", want)
		}
	}
	if strings.Contains(out, "# TYPE rezoagwe_keys counter") {
		t.Error("a level was exposed as a counter")
	}
}

func TestPrometheusLabelsPerPeerSeries(t *testing.T) {
	m := New()
	m.SentTo("10.0.0.2:3137", pb.KindHello, 60)
	m.RecvFrom("10.0.0.2:3137", pb.KindHello, 60)
	out := m.Snapshot().Prometheus()

	for _, want := range []string{
		`rezoagwe_peer_packets_sent_total{peer="10.0.0.2:3137"} 1`,
		`rezoagwe_peer_packets_received_total{peer="10.0.0.2:3137"} 1`,
		`rezoagwe_peer_bytes_sent_total{peer="10.0.0.2:3137"} 60`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition missing %q", want)
		}
	}
}

// A node with no peers must not emit an empty HELP block for the peer series.
func TestPrometheusOmitsPeerSeriesWhenThereAreNone(t *testing.T) {
	if strings.Contains(New().Snapshot().Prometheus(), "rezoagwe_peer_packets_sent_total") {
		t.Fatal("peer series emitted with no peers")
	}
}
