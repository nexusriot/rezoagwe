package proto

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/quick"
)

// EscapedLen exists to measure values too large to want a second copy of, so
// it must agree with the encoder it is standing in for — exactly, for every
// input, or it is not a size check but a guess.
func TestEscapedLenMatchesTheEncoder(t *testing.T) {
	cases := []string{
		"", "plain", `has "quotes"`, `back\slash`, "new\nline", "tab\there",
		"carriage\rreturn", "bell\x07", "nul\x00", "<html>&amp;</html>",
		strings.Repeat("\x00", 64), "mixed \x01<\">\\\n text", "héllo wörld 日本語 🎉",
	}
	for _, s := range cases {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal %q: %v", s, err)
		}
		if got := EscapedLen(s); got != len(b) {
			t.Errorf("EscapedLen(%q) = %d, encoder produced %d (%s)", s, got, len(b), b)
		}
	}
}

// The table above is the cases someone thought of; this is the ones nobody did.
func TestEscapedLenMatchesTheEncoderForArbitraryStrings(t *testing.T) {
	if err := quick.Check(func(s string) bool {
		b, err := json.Marshal(s)
		return err == nil && EscapedLen(s) == len(b)
	}, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatal(err)
	}
}

// Bytes that are not valid UTF-8 are the case that matters most: the encoder
// turns each one into U+FFFD, six bytes written out, and a value arriving
// through the HTTP gateway is whatever bytes the client sent. Undercounting
// them by six is how a binary blob slips past a size check and strands itself.
func TestEscapedLenCountsInvalidUTF8AsTheEncoderWritesIt(t *testing.T) {
	for _, s := range []string{
		"\xff", "\x80\x81", "ok\xffbad", string([]byte{0xC3}), string([]byte{0xED, 0xA0, 0x80}),
		" ", " ", strings.Repeat("\xfe", 128),
	} {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal %q: %v", s, err)
		}
		if got := EscapedLen(s); got != len(b) {
			t.Errorf("EscapedLen(%q) = %d, encoder produced %d (%s)", s, got, len(b), b)
		}
	}
}

// Random runes never produce a broken encoding; random bytes do.
func TestEscapedLenMatchesTheEncoderForArbitraryBytes(t *testing.T) {
	if err := quick.Check(func(b []byte) bool {
		s := string(b)
		enc, err := json.Marshal(s)
		return err == nil && EscapedLen(s) == len(enc)
	}, &quick.Config{MaxCount: 4000}); err != nil {
		t.Fatal(err)
	}
}

// The escape table is derived, not copied, so this re-derives it from the
// encoder and fails if a toolchain ever disagrees about a single byte.
func TestEveryASCIIByteCostsWhatTheEncoderCharges(t *testing.T) {
	for c := 0; c < 0x80; c++ {
		s := string([]byte{byte(c)})
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal %#x: %v", c, err)
		}
		if got := EscapedLen(s); got != len(b) {
			t.Errorf("byte %#x: EscapedLen = %d, encoder produced %d (%s)", c, got, len(b), b)
		}
	}
}
