package proto

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
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

// The value hash is what lets a consistency check tell apart two replicas that
// agree on the version but hold different data (§14). A port that drops it
// turns that divergence back into apparent agreement — which the JavaScript
// codec did, because its encoder is an explicit allowlist.
//
// It must also stay *off* the anti-entropy digests, which share this shape and
// never set one: 64 characters per entry there would shrink what fits in a
// datagram for no gain.
func TestKeyVersionCarriesItsValueHashAndOmitsItWhenEmpty(t *testing.T) {
	hash := strings.Repeat("a", 64)
	withHash, err := json.Marshal(KeyVersion{
		Key: "k", Version: Version{Counter: 3, Node: "node-a"}, ValueHash: hash,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(withHash), `"vhash":"`+hash+`"`) {
		t.Errorf("the hash is not on the wire: %s", withHash)
	}

	// A digest entry sets no hash, and must not start sending an empty one.
	bare, err := json.Marshal(KeyVersion{Key: "k", Version: Version{Counter: 3, Node: "node-a"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(bare), "vhash") {
		t.Errorf("an empty value hash must be omitted: %s", bare)
	}

	// A peer older than the field sends none, which has to decode as "cannot
	// compare" rather than failing — that is what keeps a rolling upgrade from
	// reporting every peer as divergent.
	var legacy KeyVersion
	if err := json.Unmarshal([]byte(`{"key":"k","version":{"counter":3,"node":"node-a"}}`), &legacy); err != nil {
		t.Fatalf("a peer too old to send a hash did not decode: %v", err)
	}
	if legacy.ValueHash != "" {
		t.Errorf("value hash = %q, want empty", legacy.ValueHash)
	}
}

// The fingerprint is the one thing two machines that cannot talk are told to
// compare, and all three implementations print it. It therefore has to be the
// same function everywhere — the Android and desktop ports printed the first
// four bytes of the framing key instead of a digest of it, so a phone and a Go
// node holding the *same* key reported a mismatch, which is the loudest
// possible false alarm from a check whose whole job is to be believed. It is a
// hash of the key rather than a prefix for a second reason: a prefix is four
// bytes of live key material on screen and on the clipboard.
func TestKeyFingerprintVectors(t *testing.T) {
	cases := []struct{ psk, cluster, want string }{
		{"", "rezoagwe", "a4b7437d"},
		{"s3cret", "prod", "f3e8452a"},
		{"demo", "hwtest", "b526a31e"},
	}
	for _, c := range cases {
		if got := NewCodec(c.psk, c.cluster).KeyFingerprint(); got != c.want {
			t.Errorf("KeyFingerprint(%q, %q) = %s, want %s", c.psk, c.cluster, got, c.want)
		}
	}
}

// It must not be a prefix of the key, which is what the ports did.
func TestKeyFingerprintIsNotKeyMaterial(t *testing.T) {
	key := DeriveKey("demo", "hwtest")
	fp := NewCodec("demo", "hwtest").KeyFingerprint()
	if fp == hex.EncodeToString(key[:4]) {
		t.Fatal("the fingerprint is the first four bytes of the framing key")
	}
}
