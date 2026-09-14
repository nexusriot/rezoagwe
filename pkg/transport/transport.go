// Package transport abstracts how rezoagwe packets and streams reach a peer.
//
// Two implementations exist: UDP/TCP for real nodes, and an in-memory network
// with configurable loss, duplication, delay and partitions so multi-node
// convergence can be tested without sockets, sleeps or flakiness.
package transport

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
)

// Packet is one datagram as observed by the receiver. From is the source
// address the transport saw, which is not necessarily the address the sender
// advertises inside the message body (a node behind NAT, or one bound to a
// wildcard address, reports a different one) — protocol code trusts the body.
type Packet struct {
	From string
	Data []byte
}

// Transport is the packet + stream interface the node engine talks to.
//
// Packets are unreliable and size-bounded; streams are reliable and used for
// payloads that do not fit a datagram (state sync, bootstrap rosters).
type Transport interface {
	// Send delivers a datagram. A nil error means "handed to the network",
	// never "delivered".
	Send(addr string, data []byte) error
	// Packets yields inbound datagrams until the transport is closed.
	Packets() <-chan Packet
	// Dial opens a reliable stream to addr.
	Dial(addr string) (net.Conn, error)
	// Streams yields inbound streams until the transport is closed.
	Streams() <-chan net.Conn
	// LocalAddr is the address peers should use to reach this transport.
	LocalAddr() string
	// StreamsAvailable reports whether Dial and Streams can be used. A UDP
	// transport whose TCP listener was refused still works, falling back to
	// datagram state sync — but a large store then takes several gossip rounds
	// to converge, which is a diagnosis nobody can make from a counter.
	StreamsAvailable() bool
	Close() error
}

// MaxFrameSize caps a single stream frame, so a malicious or corrupt length
// prefix cannot make a node allocate unbounded memory.
const MaxFrameSize = 64 << 20

var ErrFrameTooLarge = errors.New("frame exceeds maximum size")

// WriteFrame writes a length-prefixed frame. Streams carry the same
// authenticated frames as datagrams; only the length prefix is added, so a
// payload larger than a datagram needs no separate chunking protocol.
func WriteFrame(w io.Writer, data []byte) error {
	if len(data) > MaxFrameSize {
		return ErrFrameTooLarge
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(data)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

// ReadFrame reads one length-prefixed frame.
func ReadFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrameSize {
		return nil, ErrFrameTooLarge
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
