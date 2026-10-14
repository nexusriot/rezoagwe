package node

import (
	"net"
	"testing"
)

// A node told to find peers on a LAN must not advertise an address that only
// means something on the machine it runs on. Without this it is known by two
// names — the one it gossips and the one it publishes — and the cluster hands
// it its own address back as a peer.
func TestMDNSResolvesAWildcardAdvertiseAddress(t *testing.T) {
	local := LocalIPv4()
	if local == "" {
		t.Skip("no routable IPv4 address on this machine")
	}

	for _, bind := range []string{":3137", "0.0.0.0:3137"} {
		cfg := Config{NodeAddr: bind, MDNS: true}
		cfg.applyDefaults()
		want := net.JoinHostPort(local, "3137")
		if cfg.AdvertiseAddr != want {
			t.Errorf("bind %q advertised %q, want %q", bind, cfg.AdvertiseAddr, want)
		}
	}
}

// An explicit -advertise is a decision; discovery must not overrule it.
func TestMDNSLeavesAnExplicitAdvertiseAlone(t *testing.T) {
	cfg := Config{NodeAddr: ":3137", AdvertiseAddr: "10.9.9.9:4000", MDNS: true}
	cfg.applyDefaults()
	if cfg.AdvertiseAddr != "10.9.9.9:4000" {
		t.Errorf("advertise = %q, want the configured address", cfg.AdvertiseAddr)
	}
}

// Without -mdns nothing changes: a node on one machine advertising ":3137" is
// correct, and rewriting it would be a behaviour change for every user.
func TestWildcardAdvertiseIsUntouchedWithoutMDNS(t *testing.T) {
	cfg := Config{NodeAddr: ":3137"}
	cfg.applyDefaults()
	if cfg.AdvertiseAddr != ":3137" {
		t.Errorf("advertise = %q, want :3137 unchanged", cfg.AdvertiseAddr)
	}
}

func TestUnspecifiedHost(t *testing.T) {
	for host, want := range map[string]bool{
		"":          true,
		"0.0.0.0":   true,
		"::":        true,
		"127.0.0.1": false,
		"10.0.0.4":  false,
		"example":   false,
	} {
		if got := unspecifiedHost(host); got != want {
			t.Errorf("unspecifiedHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestSplitHostPort(t *testing.T) {
	host, port := splitHostPort("10.0.0.4:3137")
	if host != "10.0.0.4" || port != 3137 {
		t.Errorf("got %q/%d, want 10.0.0.4/3137", host, port)
	}
	if h, p := splitHostPort(":3137"); h != "" || p != 3137 {
		t.Errorf("got %q/%d, want \"\"/3137", h, p)
	}
	if _, p := splitHostPort("nonsense"); p != 0 {
		t.Errorf("port = %d, want 0 for an unparseable address", p)
	}
}
