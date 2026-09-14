package transport

import (
	"errors"
	"math/rand"
	"net"
	"sync"
	"time"
)

var (
	ErrNoRoute = errors.New("no route to peer")
	ErrClosed  = errors.New("transport closed")
)

// MemNet is an in-process network used by tests. It models the properties that
// actually break a gossip protocol — datagram loss, duplication, delay and
// partitions — so convergence can be asserted deterministically instead of
// hoped for against a real socket.
//
// Streams are always reliable when the peers are not partitioned, matching TCP.
type MemNet struct {
	mu         sync.Mutex
	rnd        *rand.Rand
	nodes      map[string]*MemTransport
	loss       float64
	dup        float64
	delay      time.Duration
	partitions map[string]bool

	sent    int
	dropped int
}

// NewMemNet seeds its own generator so a failing convergence test reproduces.
func NewMemNet(seed int64) *MemNet {
	return &MemNet{
		rnd:        rand.New(rand.NewSource(seed)),
		nodes:      make(map[string]*MemTransport),
		partitions: make(map[string]bool),
	}
}

// SetLoss sets the fraction of datagrams dropped in flight (0..1).
func (n *MemNet) SetLoss(f float64) {
	n.mu.Lock()
	n.loss = f
	n.mu.Unlock()
}

// SetDuplication sets the fraction of datagrams delivered twice.
func (n *MemNet) SetDuplication(f float64) {
	n.mu.Lock()
	n.dup = f
	n.mu.Unlock()
}

// SetDelay adds latency to every datagram.
func (n *MemNet) SetDelay(d time.Duration) {
	n.mu.Lock()
	n.delay = d
	n.mu.Unlock()
}

// Partition cuts (or restores) the link between two addresses in both
// directions.
func (n *MemNet) Partition(a, b string, cut bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.partitions[a+"|"+b] = cut
	n.partitions[b+"|"+a] = cut
}

// Stats reports datagrams handed to the network and datagrams dropped by it.
func (n *MemNet) Stats() (sent, dropped int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.sent, n.dropped
}

// Node returns the transport bound to addr, creating it on first use.
func (n *MemNet) Node(addr string) *MemTransport {
	n.mu.Lock()
	defer n.mu.Unlock()
	if t, ok := n.nodes[addr]; ok {
		return t
	}
	t := &MemTransport{
		net:     n,
		addr:    addr,
		packets: make(chan Packet, 1024),
		streams: make(chan net.Conn, 16),
	}
	n.nodes[addr] = t
	return t
}

func (n *MemNet) lookup(addr string) *MemTransport {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.nodes[addr]
}

func (n *MemNet) blocked(from, to string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.partitions[from+"|"+to]
}

// roll decides the fate of one datagram under the current loss/duplication
// settings, and reports the delay to apply.
func (n *MemNet) roll() (drop bool, copies int, delay time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent++
	if n.loss > 0 && n.rnd.Float64() < n.loss {
		n.dropped++
		return true, 0, 0
	}
	copies = 1
	if n.dup > 0 && n.rnd.Float64() < n.dup {
		copies = 2
	}
	return false, copies, n.delay
}

func (n *MemNet) remove(addr string) {
	n.mu.Lock()
	delete(n.nodes, addr)
	n.mu.Unlock()
}

// MemTransport is one endpoint on a MemNet.
//
// Delivery runs on the *sender's* goroutine (or a timer's, when a delay is
// set), so the closed flag is guarded rather than signalled by closing the
// channels from elsewhere — otherwise a datagram in flight during shutdown
// would send on a closed channel.
type MemTransport struct {
	net     *MemNet
	addr    string
	packets chan Packet
	streams chan net.Conn

	mu     sync.RWMutex
	closed bool
}

func (t *MemTransport) LocalAddr() string { return t.addr }

func (t *MemTransport) Packets() <-chan Packet { return t.packets }

// StreamsAvailable is always true: the in-memory network has no listener to
// fail to bind.
func (t *MemTransport) StreamsAvailable() bool { return true }

func (t *MemTransport) Streams() <-chan net.Conn { return t.streams }

func (t *MemTransport) Send(addr string, data []byte) error {
	t.mu.RLock()
	closed := t.closed
	t.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	if t.net.blocked(t.addr, addr) {
		return nil // a partition looks like loss to the sender, not an error
	}
	peer := t.net.lookup(addr)
	if peer == nil {
		return nil // nothing listening: also indistinguishable from loss
	}
	drop, copies, delay := t.net.roll()
	if drop {
		return nil
	}
	buf := make([]byte, len(data))
	copy(buf, data)
	deliver := func() {
		for i := 0; i < copies; i++ {
			peer.deliver(Packet{From: t.addr, Data: buf})
		}
	}
	if delay > 0 {
		time.AfterFunc(delay, deliver)
	} else {
		deliver()
	}
	return nil
}

func (t *MemTransport) deliver(p Packet) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.closed {
		return
	}
	select {
	case t.packets <- p:
	default: // receiver is not draining: the kernel would drop this too
	}
}

func (t *MemTransport) accept(conn net.Conn) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.closed {
		return false
	}
	select {
	case t.streams <- conn:
		return true
	case <-time.After(dialTimeout):
		return false
	}
}

func (t *MemTransport) Dial(addr string) (net.Conn, error) {
	if t.net.blocked(t.addr, addr) {
		return nil, ErrNoRoute
	}
	peer := t.net.lookup(addr)
	if peer == nil {
		return nil, ErrNoRoute
	}
	client, server := net.Pipe()
	if !peer.accept(server) {
		client.Close()
		server.Close()
		return nil, ErrNoRoute
	}
	return client, nil
}

func (t *MemTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	close(t.packets)
	close(t.streams)
	t.mu.Unlock()
	t.net.remove(t.addr)
	return nil
}
