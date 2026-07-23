package proto

import (
	"encoding/json"
	"testing"
)

func TestGoodbyeRoundTrip(t *testing.T) {
	in := Goodbye{From: ":3137", Nick: "alice"}

	pkt, err := EncodeJSON(KindGoodbye, in)
	if err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	if pkt[0] != byte(KindGoodbye) {
		t.Fatalf("envelope kind byte = %d, want %d", pkt[0], KindGoodbye)
	}

	kind, body, err := SplitKind(pkt)
	if err != nil {
		t.Fatalf("SplitKind: %v", err)
	}
	if kind != KindGoodbye {
		t.Fatalf("kind = %d, want %d", kind, KindGoodbye)
	}

	var out Goodbye
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip = %+v, want %+v", out, in)
	}
}

// A nick-less Goodbye must omit the field on the wire and decode to an empty
// nick, since peers fall back to their own last-known nick in that case.
func TestGoodbyeOmitsEmptyNick(t *testing.T) {
	pkt, err := EncodeJSON(KindGoodbye, Goodbye{From: ":3137"})
	if err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	_, body, err := SplitKind(pkt)
	if err != nil {
		t.Fatalf("SplitKind: %v", err)
	}
	if got := string(body); got != `{"from":":3137"}` {
		t.Fatalf("body = %s, want %s", got, `{"from":":3137"}`)
	}
}

func TestSplitKindEmpty(t *testing.T) {
	if _, _, err := SplitKind(nil); err == nil {
		t.Fatal("SplitKind(nil): expected error, got nil")
	}
}

func TestVersionNewer(t *testing.T) {
	cases := []struct {
		name     string
		a, b     Version
		wantANew bool
	}{
		{"higher counter wins", Version{Counter: 2, Node: "a"}, Version{Counter: 1, Node: "z"}, true},
		{"lower counter loses", Version{Counter: 1, Node: "z"}, Version{Counter: 2, Node: "a"}, false},
		{"equal counter, node tiebreak", Version{Counter: 1, Node: "b"}, Version{Counter: 1, Node: "a"}, true},
		{"identical is not newer", Version{Counter: 1, Node: "a"}, Version{Counter: 1, Node: "a"}, false},
	}
	for _, tc := range cases {
		if got := tc.a.Newer(tc.b); got != tc.wantANew {
			t.Fatalf("%s: Newer = %v, want %v", tc.name, got, tc.wantANew)
		}
	}
}
