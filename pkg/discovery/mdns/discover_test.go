package mdns

import (
	"net"
	"testing"
	"time"
)

// collectFrom runs one response through the collector and reports what it
// learned, without any sockets.
func collectFrom(t *testing.T, self string, answers []record) []Entry {
	t.Helper()
	s := &Server{cfg: Config{Instance: self}, entries: map[string]Entry{}}
	s.collect(&message{response: true, answers: answers})
	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e)
	}
	return out
}

func TestCollectAssemblesAnEntryFromItsRecords(t *testing.T) {
	got := collectFrom(t, "self", []record{
		{name: Service, rtype: typePTR, ptr: "node-a." + Service},
		{name: "node-a." + Service, rtype: typeSRV, target: "node-a.local.", port: 3137},
		{name: "node-a." + Service, rtype: typeTXT, txt: []string{"cluster=rezoagwe", "nick=alice"}},
		{name: "node-a.local.", rtype: typeA, ip: net.IPv4(10, 0, 0, 4)},
	})
	if len(got) != 1 {
		t.Fatalf("collected %d entries, want 1: %+v", len(got), got)
	}
	e := got[0]
	if e.Addr != "10.0.0.4:3137" {
		t.Errorf("addr = %q, want 10.0.0.4:3137", e.Addr)
	}
	if e.Cluster != "rezoagwe" || e.Nick != "alice" {
		t.Errorf("entry = %+v, want the TXT values", e)
	}
}

// Found on a real LAN: a phone advertising a nearby-presence service answered
// the browse, and its SRV record was collected as a rezoagwe node.
func TestCollectIgnoresOtherServices(t *testing.T) {
	got := collectFrom(t, "self", []record{
		{name: "_nearbypresence._tcp.local.", rtype: typePTR, ptr: "phone._nearbypresence._tcp.local."},
		{name: "phone._nearbypresence._tcp.local.", rtype: typeSRV, target: "phone.local.", port: 35963},
		{name: "phone.local.", rtype: typeA, ip: net.IPv4(192, 168, 88, 36)},
	})
	if len(got) != 0 {
		t.Errorf("collected a foreign service as a peer: %+v", got)
	}
}

func TestCollectIgnoresOurOwnAnnouncement(t *testing.T) {
	got := collectFrom(t, "node-a", []record{
		{name: Service, rtype: typePTR, ptr: "node-a." + Service},
		{name: "node-a." + Service, rtype: typeSRV, target: "node-a.local.", port: 3137},
		{name: "node-a.local.", rtype: typeA, ip: net.IPv4(10, 0, 0, 4)},
	})
	if len(got) != 0 {
		t.Errorf("collected our own announcement, echoed back by the group: %+v", got)
	}
}

// A PTR says a node exists; without the SRV and A there is nowhere to send
// anything, and inventing an address is worse than waiting for the next browse.
func TestCollectNeedsAnAddressNotJustAName(t *testing.T) {
	got := collectFrom(t, "self", []record{
		{name: Service, rtype: typePTR, ptr: "node-a." + Service},
	})
	if len(got) != 0 {
		t.Errorf("collected an entry with no address: %+v", got)
	}

	got = collectFrom(t, "self", []record{
		{name: Service, rtype: typePTR, ptr: "node-a." + Service},
		{name: "node-a." + Service, rtype: typeSRV, target: "node-a.local.", port: 3137},
	})
	if len(got) != 0 {
		t.Errorf("collected an SRV whose target never resolved: %+v", got)
	}
}

// The live path, over a real multicast socket. Skipped where the network will
// not carry it — a container with no multicast is a reason to skip, not to
// fail, and the node treats it the same way.
func TestBrowseFindsAnotherResponder(t *testing.T) {
	a, err := New(Config{Instance: "test-node-a", Port: 3137, Cluster: "rezoagwe-test", Nick: "alice",
		IP: net.IPv4(10, 0, 0, 4)})
	if err != nil {
		t.Skipf("multicast unavailable: %v", err)
	}
	defer a.Close()

	b, err := New(Config{Instance: "test-node-b", Port: 3138, Cluster: "rezoagwe-test", Nick: "bob",
		IP: net.IPv4(10, 0, 0, 5)})
	if err != nil {
		t.Skipf("multicast unavailable: %v", err)
	}
	defer b.Close()

	var found *Entry
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && found == nil {
		for _, e := range b.Browse(mdnsTestWait) {
			if e.Instance == "test-node-a."+Service {
				entry := e
				found = &entry
			}
		}
	}
	if found == nil {
		t.Skip("no answer on this network; multicast is not usable here")
	}
	if found.Addr != "10.0.0.4:3137" {
		t.Errorf("addr = %q, want the advertised 10.0.0.4:3137", found.Addr)
	}
	if found.Cluster != "rezoagwe-test" || found.Nick != "alice" {
		t.Errorf("entry = %+v, want the advertised cluster and nick", *found)
	}
}

const mdnsTestWait = 1500 * time.Millisecond

// A node must not offer itself as a peer.
func TestBrowseNeverReturnsItself(t *testing.T) {
	s, err := New(Config{Instance: "test-lonely", Port: 3137, Cluster: "rezoagwe-test",
		IP: net.IPv4(10, 0, 0, 9)})
	if err != nil {
		t.Skipf("multicast unavailable: %v", err)
	}
	defer s.Close()
	s.Announce()

	for _, e := range s.Browse(mdnsTestWait) {
		if e.Instance == s.instanceName() {
			t.Errorf("browse returned this node itself: %+v", e)
		}
	}
}
