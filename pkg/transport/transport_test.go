package transport

import (
	"bytes"
	"testing"
	"time"
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
