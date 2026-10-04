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
	"time"
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
	// never "delivered". A payload larger than MaxDatagramPayload is refused
	// with ErrPacketTooLarge, which asks the caller for a stream rather than
	// reporting a network failure.
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

// MaxDatagramPayload is the largest payload a datagram can carry: 65535 less
// the 8-byte UDP and 20-byte IPv4 headers.
//
// Send checks this itself rather than letting the kernel reject the write,
// because the kernel's refusal is a platform-specific errno (EMSGSIZE on Unix,
// WSAEMSGSIZE on Windows) that the caller cannot portably recognise — and the
// caller has to recognise it, since the right answer is to put the same frame
// on a stream, not to count a send error and drop the update.
const MaxDatagramPayload = 65507

var ErrFrameTooLarge = errors.New("frame exceeds maximum size")

// ErrPacketTooLarge reports a payload no datagram can carry. It means "use a
// stream", not "the network failed".
var ErrPacketTooLarge = errors.New("packet exceeds maximum datagram payload")

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

// MinStreamThroughput is the slowest link a stream deadline assumes, in bytes
// per second. Deliberately pessimistic: well under a weak Wi-Fi link, so the
// allowance is generous rather than a second guess at the network.
const MinStreamThroughput = 1 << 20

// StreamDeadline is how long one stream operation gets for a payload of n
// bytes: a fixed base, plus an allowance at MinStreamThroughput.
//
// A fixed timeout is a throughput assumption in disguise. Ten seconds is
// generous for a digest and impossible for a value of tens of megabytes, so
// the replication ceiling promised something the deadline then refused: the
// frame went out, the clock ran out mid-transfer, and the entry never
// replicated — with nothing but a stream-error counter to show for it.
// Measured on a Wi-Fi LAN, 30 MB to a tablet moved inside the fixed window
// and 64 MB did not.
//
// Go is the only implementation that needed this. A Node socket timeout and a
// Java SO_TIMEOUT both measure inactivity and reset as bytes move; only
// SetDeadline bounds the whole transfer, which is the one meaning that gets
// shorter as the payload grows.
func StreamDeadline(base time.Duration, n int) time.Duration {
	if n <= 0 {
		return base
	}
	return base + time.Duration(n/MinStreamThroughput)*time.Second
}

// ReadFrameFrom reads one frame, extending the connection's deadline once the
// length prefix says how much is coming.
//
// A deadline set before the size is known is a guess: a state sync carrying a
// large store outgrows it and fails halfway, and the half that arrived is
// indistinguishable from a peer that never answered.
func ReadFrameFrom(conn net.Conn, base time.Duration) ([]byte, error) {
	if err := conn.SetReadDeadline(time.Now().Add(base)); err != nil {
		return nil, err
	}
	var hdr [4]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrameSize {
		return nil, ErrFrameTooLarge
	}
	if err := conn.SetReadDeadline(time.Now().Add(StreamDeadline(base, int(n)))); err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// WriteFrameTo writes one frame, giving the connection a deadline in
// proportion to what it carries.
func WriteFrameTo(conn net.Conn, base time.Duration, data []byte) error {
	if err := conn.SetWriteDeadline(time.Now().Add(StreamDeadline(base, len(data)))); err != nil {
		return err
	}
	return WriteFrame(conn, data)
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
