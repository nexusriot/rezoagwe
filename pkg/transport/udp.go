package transport

import (
	"errors"
	"net"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// maxDatagram is the largest datagram we are willing to read. Anything larger
// cannot be sent over UDP anyway.
const maxDatagram = 65535

// dialTimeout bounds a stream dial so an unreachable peer cannot stall the
// caller (state sync runs on a background goroutine, but a joiner waits on it).
const dialTimeout = 5 * time.Second

// UDPTransport sends and receives datagrams on a single shared socket, and
// accepts reliable streams on the same address over TCP.
//
// One socket for both directions matters: opening a fresh socket per packet
// (as this code used to) burns a file descriptor and an ephemeral port per
// message, and makes every packet appear to come from a different port.
type UDPTransport struct {
	conn     *net.UDPConn
	listener net.Listener
	addr     string

	packets chan Packet
	streams chan net.Conn

	resolveMu sync.Mutex
	resolved  map[string]*net.UDPAddr

	closeOnce sync.Once
	closed    chan struct{}
}

// ListenUDP binds addr for datagrams and, when possible, the same address for
// streams. A failure to bind the stream listener is not fatal: the node still
// works, falling back to datagram state sync.
func ListenUDP(addr string) (*UDPTransport, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}
	t := &UDPTransport{
		conn:     conn,
		addr:     addr,
		packets:  make(chan Packet, 256),
		streams:  make(chan net.Conn, 16),
		resolved: make(map[string]*net.UDPAddr),
		closed:   make(chan struct{}),
	}
	if ln, err := net.Listen("tcp", addr); err != nil {
		log.Errorf("stream listener on %s unavailable, falling back to datagram state sync: %s", addr, err)
	} else {
		t.listener = ln
		go t.acceptLoop()
	}
	go t.readLoop()
	return t, nil
}

func (t *UDPTransport) LocalAddr() string { return t.addr }

func (t *UDPTransport) Packets() <-chan Packet { return t.packets }

func (t *UDPTransport) Streams() <-chan net.Conn { return t.streams }

func (t *UDPTransport) Send(addr string, data []byte) error {
	udpAddr, err := t.resolve(addr)
	if err != nil {
		return err
	}
	_, err = t.conn.WriteToUDP(data, udpAddr)
	return err
}

func (t *UDPTransport) Dial(addr string) (net.Conn, error) {
	if t.listener == nil {
		return nil, errors.New("stream transport unavailable")
	}
	return net.DialTimeout("tcp", addr, dialTimeout)
}

func (t *UDPTransport) Close() error {
	var err error
	t.closeOnce.Do(func() {
		close(t.closed)
		if t.listener != nil {
			t.listener.Close()
		}
		err = t.conn.Close()
	})
	return err
}

// resolve caches address lookups: peer addresses repeat on every gossip tick,
// and resolving each time turns a hostname peer into a DNS query per packet.
func (t *UDPTransport) resolve(addr string) (*net.UDPAddr, error) {
	t.resolveMu.Lock()
	defer t.resolveMu.Unlock()
	if a, ok := t.resolved[addr]; ok {
		return a, nil
	}
	a, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	t.resolved[addr] = a
	return a, nil
}

func (t *UDPTransport) readLoop() {
	defer close(t.packets)
	buf := make([]byte, maxDatagram)
	for {
		n, from, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Errorf("read udp: %s", err)
			continue
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		pkt := Packet{From: from.String(), Data: data}
		select {
		case t.packets <- pkt:
		case <-t.closed:
			return
		}
	}
}

func (t *UDPTransport) acceptLoop() {
	defer close(t.streams)
	for {
		conn, err := t.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Errorf("accept stream: %s", err)
			continue
		}
		select {
		case t.streams <- conn:
		case <-t.closed:
			conn.Close()
			return
		}
	}
}
