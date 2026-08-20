package proto

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestCodecRoundTrip(t *testing.T) {
	c := NewCodec("secret", "prod")
	frame, err := c.Encode(KindChat, ChatMessage{Sender: ":3137", Text: "hi"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	// A fresh codec with the same key stands in for the receiving node.
	kind, body, err := NewCodec("secret", "prod").Decode(frame)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if kind != KindChat {
		t.Fatalf("kind = %s, want chat", kind)
	}
	var got ChatMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Text != "hi" {
		t.Fatalf("text = %q, want %q", got.Text, "hi")
	}
}

// A different pre-shared key must not authenticate: this is the whole point of
// the psk.
func TestCodecRejectsWrongKey(t *testing.T) {
	frame, _ := NewCodec("secret", "prod").Encode(KindHello, Hello{From: ":1"})
	if _, _, err := NewCodec("other", "prod").Decode(frame); !errors.Is(err, ErrBadMAC) {
		t.Fatalf("decode with wrong psk: err = %v, want ErrBadMAC", err)
	}
}

// Two clusters sharing a LAN must not mix, even when neither sets a psk.
func TestCodecSeparatesClusters(t *testing.T) {
	frame, _ := NewCodec("", "alpha").Encode(KindHello, Hello{From: ":1"})
	if _, _, err := NewCodec("", "beta").Decode(frame); !errors.Is(err, ErrBadMAC) {
		t.Fatalf("cross-cluster decode: err = %v, want ErrBadMAC", err)
	}
	if _, _, err := NewCodec("", "alpha").Decode(frame); err != nil {
		t.Fatalf("same-cluster decode: %v", err)
	}
}

func TestCodecTamperDetection(t *testing.T) {
	c := NewCodec("secret", "prod")
	frame, _ := c.Encode(KindKV, KVUpdate{Key: "k", Value: "v"})
	frame[len(frame)-1] ^= 0xff // flip a bit in the body

	if _, _, err := NewCodec("secret", "prod").Decode(frame); !errors.Is(err, ErrBadMAC) {
		t.Fatalf("tampered frame: err = %v, want ErrBadMAC", err)
	}
}

// A captured packet replayed later must be refused, or an observer could
// re-apply a delete (or a chat message) at will.
func TestCodecRejectsReplay(t *testing.T) {
	sender := NewCodec("secret", "prod")
	receiver := NewCodec("secret", "prod")
	frame, _ := sender.Encode(KindKV, KVUpdate{Key: "k"})

	if _, _, err := receiver.Decode(frame); err != nil {
		t.Fatalf("first decode: %v", err)
	}
	if _, _, err := receiver.Decode(frame); !errors.Is(err, ErrReplay) {
		t.Fatalf("replayed frame: err = %v, want ErrReplay", err)
	}
}

// Packets far outside the accepted skew are refused, which is what bounds how
// long a captured packet stays useful.
func TestCodecRejectsClockSkew(t *testing.T) {
	sender := NewCodec("secret", "prod")
	sender.now = func() time.Time { return time.Now().Add(-10 * time.Minute) }
	frame, _ := sender.Encode(KindHello, Hello{From: ":1"})

	if _, _, err := NewCodec("secret", "prod").Decode(frame); !errors.Is(err, ErrClockSkew) {
		t.Fatalf("stale frame: err = %v, want ErrClockSkew", err)
	}
}

// The replay cache must not grow without bound: nonces older than twice the
// skew can never be accepted again, so they are dropped.
func TestCodecPrunesReplayCache(t *testing.T) {
	c := NewCodec("secret", "prod")
	base := time.Now()
	c.now = func() time.Time { return base }
	for i := 0; i < 50; i++ {
		frame, _ := c.Encode(KindHello, Hello{From: ":1"})
		if _, _, err := c.Decode(frame); err != nil {
			t.Fatalf("decode %d: %v", i, err)
		}
	}
	if got := len(c.seen); got != 50 {
		t.Fatalf("cache size = %d, want 50 before pruning", got)
	}

	// Move well past the retention window and decode once more.
	c.now = func() time.Time { return base.Add(5 * DefaultSkew) }
	frame, _ := c.Encode(KindHello, Hello{From: ":1"})
	if _, _, err := c.Decode(frame); err != nil {
		t.Fatalf("decode after prune: %v", err)
	}
	if got := len(c.seen); got != 1 {
		t.Fatalf("cache size = %d after pruning, want 1", got)
	}
}

func TestCodecRejectsShortFrame(t *testing.T) {
	if _, _, err := NewCodec("", "").Decode([]byte{1, 2, 3}); !errors.Is(err, ErrShortFrame) {
		t.Fatalf("short frame: err = %v, want ErrShortFrame", err)
	}
}

func TestVersionNewer(t *testing.T) {
	cases := []struct {
		name string
		a, b Version
		want bool
	}{
		{"higher counter wins", Version{2, "a"}, Version{1, "z"}, true},
		{"lower counter loses", Version{1, "z"}, Version{2, "a"}, false},
		{"tie broken by node id", Version{1, "b"}, Version{1, "a"}, true},
		{"equal is not newer", Version{1, "a"}, Version{1, "a"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.Newer(tc.b); got != tc.want {
				t.Fatalf("Newer = %v, want %v", got, tc.want)
			}
		})
	}
}

// A node upgraded from wire v1 must still be able to read its own data file,
// where chat history was a list of pre-rendered strings.
func TestChatLogAcceptsLegacyStrings(t *testing.T) {
	var log ChatLog
	if err := json.Unmarshal([]byte(`["[gray]12:00[-] [red]bob[-]: hello"]`), &log); err != nil {
		t.Fatalf("unmarshal legacy chat: %v", err)
	}
	if len(log) != 1 {
		t.Fatalf("entries = %d, want 1", len(log))
	}
	if log[0].Text != "12:00 bob: hello" {
		t.Fatalf("text = %q, want colour tags stripped", log[0].Text)
	}
	if log[0].Kind != ChatSystem {
		t.Fatalf("kind = %q, want system", log[0].Kind)
	}
}

func TestChatLogRoundTrip(t *testing.T) {
	in := ChatLog{{TS: 5, Sender: ":1", Nick: "bob", Text: "hi", Kind: ChatMsg}}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out ChatLog
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out) != 1 || out[0].Nick != "bob" || out[0].Text != "hi" {
		t.Fatalf("round trip lost data: %+v", out)
	}
}
