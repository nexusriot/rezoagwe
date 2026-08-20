package metrics

import (
	"strings"
	"testing"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

func TestSnapshotCountsByKind(t *testing.T) {
	m := New()
	m.Sent(pb.KindKV, 100)
	m.Sent(pb.KindKV, 50)
	m.Recv(pb.KindChat, 20)
	m.KVApplied.Add(3)

	s := m.Snapshot()
	if s.PacketsSent != 2 || s.BytesSent != 150 {
		t.Fatalf("sent = %d packets / %d bytes, want 2/150", s.PacketsSent, s.BytesSent)
	}
	if s.PacketsRecv != 1 || s.BytesRecv != 20 {
		t.Fatalf("received = %d packets / %d bytes, want 1/20", s.PacketsRecv, s.BytesRecv)
	}
	if s.KVApplied != 3 {
		t.Fatalf("applied = %d, want 3", s.KVApplied)
	}

	byKind := map[string]KindCount{}
	for _, k := range s.Kinds {
		byKind[k.Kind] = k
	}
	if byKind["kv"].Sent != 2 {
		t.Fatalf("kv sent = %d, want 2", byKind["kv"].Sent)
	}
	if byKind["chat"].Recv != 1 {
		t.Fatalf("chat received = %d, want 1", byKind["chat"].Recv)
	}
	if _, ok := byKind["hello"]; ok {
		t.Fatal("untouched kinds should not appear in the snapshot")
	}
}

func TestPrometheusFormat(t *testing.T) {
	m := New()
	m.Sent(pb.KindDigest, 10)
	m.AuthFailures.Add(2)

	out := m.Snapshot().Prometheus()
	for _, want := range []string{
		"# TYPE rezoagwe_auth_failures_total counter",
		"rezoagwe_auth_failures_total 2",
		`rezoagwe_messages_total{kind="digest",direction="sent"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("exposition missing %q:\n%s", want, out)
		}
	}
}

// A nil Metrics must be usable, so a component without one does not have to
// branch on it.
func TestNilMetricsIsSafe(t *testing.T) {
	var m *Metrics
	m.Sent(pb.KindKV, 1)
	m.Recv(pb.KindKV, 1)
}
