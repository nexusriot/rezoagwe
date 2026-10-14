package node

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
	"github.com/nexusriot/rezoagwe/pkg/transport"
)

// seenAt holds the address a rendezvous service last observed this node
// arriving from, and whether it disagrees with what the node advertises.
type seenAt struct {
	mu       sync.Mutex
	observed string
}

// recordSeenAt stores the address bootstrap reported, if it told us one.
//
// This only ever records. Adopting it is ResolveAdvertise's job and happens
// before the node is built, because cfg.AdvertiseAddr is read without a lock
// from every loop in the engine and is also copied into the Model as the
// node's own identity — rewriting it under a running node would be a race in
// thirty places and would leave IsSelf answering about an address the node no
// longer uses.
func (n *Node) recordSeenAt(addr string) {
	if addr == "" {
		return
	}
	n.seen.mu.Lock()
	n.seen.observed = addr
	n.seen.mu.Unlock()
}

// SeenAt is the source address a rendezvous service last observed, or "" if
// none has answered yet or the seeds predate the field.
func (n *Node) SeenAt() string {
	n.seen.mu.Lock()
	defer n.seen.mu.Unlock()
	return n.seen.observed
}

// AdvertiseMismatch reports the observed address when it names a different
// host from the one this node advertises, and "" when they agree or when
// nothing has been observed.
//
// Hosts only. A datagram arrives from the node's own bound socket so its port
// is right, but a stream is dialled with an ephemeral source port and its port
// means nothing — and warning about a port that was never claimed to be the
// node's would be worse than saying nothing at all.
//
// A hostname that resolves to the observed address is not a mismatch: a node
// deliberately advertising a name peers can resolve is doing the right thing,
// and flagging it would train operators to ignore this.
func (n *Node) AdvertiseMismatch() string {
	observed := n.SeenAt()
	if observed == "" {
		return ""
	}
	observedHost, _, err := net.SplitHostPort(observed)
	if err != nil || observedHost == "" {
		return ""
	}
	advertisedHost, _, err := net.SplitHostPort(n.cfg.AdvertiseAddr)
	if err != nil {
		return ""
	}
	if sameHost(advertisedHost, observedHost) {
		return ""
	}
	return observed
}

// sameHost compares an advertised host with an observed one, resolving a name
// rather than calling it a mismatch.
func sameHost(advertised, observed string) bool {
	if advertised == "" {
		// A wildcard advertises nothing in particular, which is exactly the
		// case this check exists for: whatever bootstrap saw, the node is not
		// telling peers to use it.
		return false
	}
	if strings.EqualFold(advertised, observed) {
		return true
	}
	if ip := net.ParseIP(advertised); ip != nil {
		// Both are literals and they differ. An unspecified address is the
		// wildcard case again; a loopback is wrong on any network and the
		// existing rule already says so, but this one names what to use
		// instead, so it still reports.
		return false
	}
	// A name. Resolving it is a lookup on a diagnostics path, not a hot one,
	// and a name that does resolve to where bootstrap saw us is correct.
	addrs, err := net.LookupHost(advertised)
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if strings.EqualFold(a, observed) {
			return true
		}
	}
	return false
}

// ResolveAdvertise asks a rendezvous service where it sees this node from and
// returns the address to advertise: that host, with the port the node binds.
//
// It runs before node.New, not inside a running node. cfg.AdvertiseAddr is
// read without a lock by every loop in the engine and is copied into the Model
// as the node's own identity, so rewriting it later would be a race in thirty
// places and would leave IsSelf answering about an address the node had
// stopped using — which is how a node ends up adopting itself as a peer.
//
// The exchange is a stream, which is synchronous and needs no packet loop
// running. Its source port is ephemeral and deliberately discarded: only the
// host is taken, and the port comes from what this node binds.
//
// An empty return with a nil error means no seed answered with an address.
// That is not a failure — a seed may predate the field, or be unreachable —
// and the caller keeps whatever it was going to advertise anyway.
func ResolveAdvertise(cfg Config, timeout time.Duration) (string, error) {
	cfg.applyDefaults()
	_, port, err := net.SplitHostPort(cfg.NodeAddr)
	if err != nil {
		return "", fmt.Errorf("parse -node %q: %w", cfg.NodeAddr, err)
	}
	codec := pb.NewCodec(cfg.PSK, cfg.Cluster)
	req := pb.BootstrapDiscover{From: cfg.AdvertiseAddr}
	pkt, err := codec.Encode(pb.KindBootstrapDiscover, req)
	if err != nil {
		return "", err
	}

	var lastErr error
	for _, seed := range cfg.BootstrapAddrs {
		if seed == "" {
			continue
		}
		host, err := askSeed(seed, codec, pkt, timeout)
		if err != nil {
			lastErr = err
			continue
		}
		if host == "" {
			continue
		}
		return net.JoinHostPort(host, port), nil
	}
	return "", lastErr
}

// askSeed performs one Discover exchange and returns the host the seed saw.
func askSeed(seed string, codec *pb.Codec, pkt []byte, timeout time.Duration) (string, error) {
	conn, err := net.DialTimeout("tcp", seed, timeout)
	if err != nil {
		return "", fmt.Errorf("dial %s: %w", seed, err)
	}
	defer conn.Close()
	if err := transport.WriteFrameTo(conn, timeout, pkt); err != nil {
		return "", fmt.Errorf("ask %s: %w", seed, err)
	}
	frame, err := transport.ReadFrameFrom(conn, timeout)
	if err != nil {
		return "", fmt.Errorf("read from %s: %w", seed, err)
	}
	var roster pb.BootstrapRoster
	kind, err := codec.DecodeInto(frame, &roster)
	if err != nil {
		return "", fmt.Errorf("decode from %s: %w", seed, err)
	}
	if kind != pb.KindBootstrapRoster {
		return "", fmt.Errorf("%s answered %s, not a roster", seed, kind)
	}
	if roster.SeenAt == "" {
		return "", nil // an older rendezvous service; nothing to adopt
	}
	host, _, err := net.SplitHostPort(roster.SeenAt)
	if err != nil {
		return "", nil
	}
	return host, nil
}
