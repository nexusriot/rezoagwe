package proto

import (
	"encoding/json"
	"testing"
	"time"
)

// FuzzDecode drives the one function in this system that parses input chosen
// by somebody else.
//
// Every node binds a UDP port that anything on the network can send to, and
// Decode runs on the bytes before anything has established they came from a
// cluster member — that is what it is for. A panic here is a remote crash, and
// nothing else in the suite feeds it bytes it did not construct itself.
func FuzzDecode(f *testing.F) {
	c := NewCodec("psk", "cluster")

	// Seed with one well-formed frame of each shape, so the fuzzer starts from
	// inputs that get past the MAC rather than from noise that never does.
	for _, seed := range []struct {
		kind MessageKind
		body interface{}
	}{
		{KindKV, KVUpdate{Action: KVSet, Key: "k", Value: "v", Version: Version{Counter: 1, Node: "A"}}},
		{KindChat, ChatMessage{Sender: "10.0.0.1:3137", Text: "hello", Seq: 1}},
		{KindHello, Hello{From: "10.0.0.1:3137", Nick: "alice", ID: "abc"}},
		{KindDigest, Digest{From: "10.0.0.1:3137", Entries: []KeyVersion{{Key: "k"}}}},
		{KindBootstrapRoster, BootstrapRoster{Peers: []BootstrapPeer{{Addr: "10.0.0.1:3137"}}}},
	} {
		if pkt, err := c.Encode(seed.kind, seed.body); err == nil {
			f.Add(pkt)
		}
	}
	f.Add([]byte{})
	f.Add(make([]byte, headerLen))

	f.Fuzz(func(t *testing.T, frame []byte) {
		// A fresh codec per input: the replay cache would otherwise make the
		// result depend on what the fuzzer happened to try before.
		dec := NewCodec("psk", "cluster")
		kind, body, err := dec.Decode(frame)
		if err != nil {
			return
		}
		// An accepted frame must be self-consistent: it authenticated, so the
		// body is exactly the bytes the sender covered with the MAC.
		if len(frame) < headerLen {
			t.Fatalf("accepted a frame shorter than the header: %d bytes", len(frame))
		}
		if got := MessageKind(frame[0]); got != kind {
			t.Fatalf("kind = %v, want %v", kind, got)
		}
		if string(body) != string(frame[headerLen:]) {
			t.Fatal("body is not the tail of the frame it was taken from")
		}
		// And the dispatcher's first move must not panic on it. An
		// authenticated frame can still carry a body that is not the shape its
		// kind implies — a cluster member running a different version, or one
		// that has been tampered with by someone holding the key.
		var v interface{}
		_ = json.Unmarshal(body, &v)
	})
}

// FuzzEscapedLen pins the size check to the encoder for inputs nobody would
// think to write down.
//
// A value is admitted or refused on what EscapedLen says it will occupy once
// encoded. If that disagrees with the encoder by even a byte, a value can be
// accepted here and then be unreplicable everywhere — which is the one
// outcome the check exists to prevent.
func FuzzEscapedLen(f *testing.F) {
	for _, seed := range []string{"", "plain", `"quoted"`, "tab\there", "\x00\x01", "héllo", "\xff\xfe", "😀"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		encoded, err := json.Marshal(s)
		if err != nil {
			t.Skip() // not a string the encoder will take at all
		}
		if got := EscapedLen(s); got != len(encoded) {
			t.Fatalf("EscapedLen(%q) = %d, encoder produced %d (%s)", s, got, len(encoded), encoded)
		}
	})
}

// FuzzDecodeWithSkew exercises the timestamp and replay paths, which the plain
// decode fuzz never reaches: a fuzzer is not going to guess a MAC, so without
// a codec that accepts the frame the checks after it are dead code here.
func FuzzDecodeWithSkew(f *testing.F) {
	f.Add(int64(0), uint8(KindKV), []byte(`{"key":"k"}`))
	f.Add(int64(-3600), uint8(KindChat), []byte(`{"text":"x"}`))
	f.Add(int64(3600), uint8(KindHello), []byte(`{}`))

	f.Fuzz(func(t *testing.T, offsetSec int64, kind uint8, body []byte) {
		if offsetSec > 1<<40 || offsetSec < -(1<<40) {
			t.Skip() // beyond any clock a real node could report
		}
		base := time.Unix(1_700_000_000, 0)
		sender := NewCodec("psk", "cluster")
		sender.now = func() time.Time { return base.Add(time.Duration(offsetSec) * time.Second) }

		pkt, err := sender.Encode(MessageKind(kind), json.RawMessage(body))
		if err != nil {
			t.Skip() // not a body the encoder will take
		}
		receiver := NewCodec("psk", "cluster")
		receiver.now = func() time.Time { return base }

		_, _, err = receiver.Decode(pkt)
		skewed := offsetSec > int64(DefaultSkew.Seconds()) || offsetSec < -int64(DefaultSkew.Seconds())
		if skewed && err == nil {
			t.Fatalf("accepted a frame %ds out of skew", offsetSec)
		}
		if !skewed && err != nil {
			t.Fatalf("rejected a frame %ds out of skew: %v", offsetSec, err)
		}
		// A frame accepted once must never be accepted again.
		if err == nil {
			if _, _, again := receiver.Decode(pkt); again == nil {
				t.Fatal("the same nonce was accepted twice")
			}
		}
	})
}
