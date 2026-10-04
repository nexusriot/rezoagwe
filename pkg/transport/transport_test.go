package transport

import (
	"bytes"
	"net"
	"testing"
	"time"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payload := bytes.Repeat([]byte("x"), 100000) // larger than any datagram
	if err := WriteFrame(&buf, payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: %d bytes back, want %d", len(got), len(payload))
	}
}

func TestUDPTransportRoundTrip(t *testing.T) {
	a, err := ListenUDP("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen a: %v", err)
	}
	defer a.Close()
	b, err := ListenUDP("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen b: %v", err)
	}
	defer b.Close()

	if err := a.Send(b.conn.LocalAddr().String(), []byte("ping")); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case pkt := <-b.Packets():
		if string(pkt.Data) != "ping" {
			t.Fatalf("payload = %q, want ping", pkt.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no datagram delivered")
	}
}

func TestUDPTransportStream(t *testing.T) {
	a, err := ListenUDP("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen a: %v", err)
	}
	defer a.Close()
	b, err := ListenUDP("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen b: %v", err)
	}
	defer b.Close()

	go func() {
		conn := <-b.Streams()
		defer conn.Close()
		frame, err := ReadFrame(conn)
		if err != nil {
			return
		}
		WriteFrame(conn, append([]byte("echo:"), frame...))
	}()

	conn, err := a.Dial(b.listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := WriteFrame(conn, []byte("hello")); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	got, err := ReadFrame(conn)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if string(got) != "echo:hello" {
		t.Fatalf("response = %q", got)
	}
}

func TestMemNetDelivers(t *testing.T) {
	net := NewMemNet(1)
	a := net.Node("a")
	b := net.Node("b")
	defer a.Close()
	defer b.Close()

	if err := a.Send("b", []byte("hi")); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case pkt := <-b.Packets():
		if pkt.From != "a" || string(pkt.Data) != "hi" {
			t.Fatalf("packet = %+v", pkt)
		}
	default:
		t.Fatal("nothing delivered")
	}
}

// Total loss must swallow everything, which is what lets a test prove that
// anti-entropy — not the original datagram — is what repaired a store.
func TestMemNetLoss(t *testing.T) {
	net := NewMemNet(1)
	net.SetLoss(1.0)
	a, b := net.Node("a"), net.Node("b")
	defer a.Close()
	defer b.Close()

	for i := 0; i < 20; i++ {
		a.Send("b", []byte("x"))
	}
	if len(b.Packets()) != 0 {
		t.Fatalf("delivered %d packets under total loss", len(b.Packets()))
	}
	sent, dropped := net.Stats()
	if sent != 20 || dropped != 20 {
		t.Fatalf("stats = %d sent / %d dropped, want 20/20", sent, dropped)
	}
}

func TestMemNetPartition(t *testing.T) {
	net := NewMemNet(1)
	a, b := net.Node("a"), net.Node("b")
	defer a.Close()
	defer b.Close()

	net.Partition("a", "b", true)
	a.Send("b", []byte("x"))
	if len(b.Packets()) != 0 {
		t.Fatal("packet crossed a partition")
	}
	if _, err := a.Dial("b"); err == nil {
		t.Fatal("stream crossed a partition")
	}

	net.Partition("a", "b", false)
	a.Send("b", []byte("x"))
	if len(b.Packets()) != 1 {
		t.Fatal("packet not delivered after the partition healed")
	}
}

func TestMemNetStream(t *testing.T) {
	net := NewMemNet(1)
	a, b := net.Node("a"), net.Node("b")
	defer a.Close()
	defer b.Close()

	go func() {
		conn := <-b.Streams()
		defer conn.Close()
		frame, err := ReadFrame(conn)
		if err != nil {
			return
		}
		WriteFrame(conn, frame)
	}()

	conn, err := a.Dial("b")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	big := bytes.Repeat([]byte("y"), 200000)
	go WriteFrame(conn, big)
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	got, err := ReadFrame(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, big) {
		t.Fatalf("echoed %d bytes, want %d", len(got), len(big))
	}
}

// Sending into a closed endpoint must not panic on a closed channel: datagrams
// are delivered on the sender's goroutine, so shutdown races are real.
func TestMemNetSendAfterClose(t *testing.T) {
	net := NewMemNet(1)
	a, b := net.Node("a"), net.Node("b")
	b.Close()
	if err := a.Send("b", []byte("x")); err != nil {
		t.Fatalf("send to closed peer: %v", err)
	}
	a.Close()
	if err := a.Send("b", []byte("x")); err != ErrClosed {
		t.Fatalf("send from closed transport: err = %v, want ErrClosed", err)
	}
}

// proto.MaxFrameBytes is the same number as MaxFrameSize, declared where the
// store can see it without importing this package. Two copies of a constant
// drift; this is the thing that notices.
func TestTheFrameCeilingAgreesWithTheProtocol(t *testing.T) {
	if MaxFrameSize != pb.MaxFrameBytes {
		t.Fatalf("transport.MaxFrameSize = %d but proto.MaxFrameBytes = %d: a value the store "+
			"admits would be refused by the frame writer, and strand itself",
			MaxFrameSize, pb.MaxFrameBytes)
	}
	if pb.MaxReplicableValueBytes >= MaxFrameSize {
		t.Fatalf("MaxReplicableValueBytes = %d leaves no room for the key, version and envelope "+
			"inside a %d-byte frame", pb.MaxReplicableValueBytes, MaxFrameSize)
	}
}

// A deadline that does not grow with the payload is a throughput assumption.
//
// Found by re-running the end-to-end suite: a value at the replication
// ceiling was accepted, the stream carried it partway, and the fixed ten
// seconds ran out mid-transfer — so the entry never replicated and the only
// evidence was a stream-error counter. On the same Wi-Fi LAN 30 MB arrived
// and 64 MB did not, which is the shape of a timeout, not of a broken link.
func TestAStreamDeadlineGrowsWithThePayload(t *testing.T) {
	const base = 10 * time.Second

	if got := StreamDeadline(base, 0); got != base {
		t.Errorf("an empty payload should get the base deadline, got %s", got)
	}
	if got := StreamDeadline(base, 1024); got != base {
		t.Errorf("a small payload should get the base deadline, got %s", got)
	}

	// 64 MiB at the assumed floor is 64 seconds of allowance on top.
	big := StreamDeadline(base, 64<<20)
	if big != base+64*time.Second {
		t.Fatalf("64 MiB got %s, want %s", big, base+64*time.Second)
	}

	// The whole point: the largest frame the protocol allows must still get
	// enough time at the assumed floor to actually arrive.
	atCeiling := StreamDeadline(base, MaxFrameSize)
	needed := time.Duration(MaxFrameSize/MinStreamThroughput) * time.Second
	if atCeiling < needed {
		t.Fatalf("a %d-byte frame gets %s but needs %s at the assumed floor",
			MaxFrameSize, atCeiling, needed)
	}
}

// ReadFrameFrom cannot know the size before reading it, so it has to widen
// the deadline once the prefix says what is coming.
func TestReadFrameFromWidensTheDeadlineOnceTheSizeIsKnown(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	// Three megabytes, written slowly enough that a fixed one-second deadline
	// would expire partway and a scaled one would not.
	payload := bytes.Repeat([]byte("x"), 3<<20)
	go func() {
		WriteFrame(client, payload)
	}()

	got, err := ReadFrameFrom(server, time.Second)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != len(payload) {
		t.Fatalf("read %d bytes, want %d", len(got), len(payload))
	}
}
