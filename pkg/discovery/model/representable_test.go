package model

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

// rawBytes is a short run of bytes that are not valid UTF-8.
const rawBytes = "\x00\xff\xfe\x80A"

// This is the fact the refusal exists for, kept as a test so it stays a fact.
// Every JSON encoder replaces an invalid byte with U+FFFD, silently, and the
// two replicas then hold different bytes at the *same version* — which
// anti-entropy is right to call agreement and can therefore never repair.
func TestJSONMangles(t *testing.T) {
	encoded, err := json.Marshal(rawBytes)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back string
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back == rawBytes {
		t.Fatal("the encoder round-tripped invalid UTF-8; this refusal may no longer be needed")
	}
	if len(back) <= len(rawBytes) {
		t.Errorf("round trip produced %d bytes from %d; expected U+FFFD expansion", len(back), len(rawBytes))
	}
}

func TestAValueThatCannotSurviveTheWireIsRefused(t *testing.T) {
	kv := NewKVStore("n1")

	if _, err := kv.Write("blob", rawBytes, WriteOptions{}); !errors.Is(err, ErrNotRepresentable) {
		t.Fatalf("writing rawBytes returned %v, want ErrNotRepresentable", err)
	}
	if _, present := kv.Get("blob"); present {
		t.Error("a refused value was stored anyway, which is how it ends up on one replica only")
	}
	// Nothing was consumed: the clock must not advance for a write that did
	// not happen, or every later version is numbered past a ghost.
	if got := kv.Clock(); got != 0 {
		t.Errorf("clock = %d after a refused write, want 0", got)
	}
}

// A key is mangled the same way, and the result is a different key: the entry
// exists under one name here and another everywhere else.
func TestAKeyThatCannotSurviveTheWireIsRefused(t *testing.T) {
	kv := NewKVStore("n1")

	if _, err := kv.Write("key-"+rawBytes, "fine", WriteOptions{}); !errors.Is(err, ErrNotRepresentable) {
		t.Fatalf("writing a rawBytes key returned %v, want ErrNotRepresentable", err)
	}
	if len(kv.Entries()) != 0 {
		t.Errorf("store holds %+v after a refused write", kv.Entries())
	}
}

// A delete carries a key and no value, so the key check has to run on a path
// that skips the value check entirely — otherwise a tombstone lands on a
// different key on every replica.
func TestADeleteWithAnUnrepresentableKeyIsRefused(t *testing.T) {
	kv := NewKVStore("n1")
	if _, err := kv.Remove("key-"+rawBytes, WriteOptions{}); !errors.Is(err, ErrNotRepresentable) {
		t.Fatalf("deleting a rawBytes key returned %v, want ErrNotRepresentable", err)
	}
	// And a delete of an ordinary key still works: the value is empty on this
	// path, and an empty value must not be read as unrepresentable.
	kv.Set("ordinary", "v")
	if _, err := kv.Remove("ordinary", WriteOptions{}); err != nil {
		t.Fatalf("deleting an ordinary key returned %v", err)
	}
}

func TestARemoteUpdateThatCannotSurviveTheWireIsRefused(t *testing.T) {
	kv := NewKVStore("n1")
	applied, err := kv.ApplyWithReason(pb.KVUpdate{
		Action: pb.KVSet, Key: "blob", Value: rawBytes,
		Version: pb.Version{Counter: 9, Node: "peer"},
	})
	if applied {
		t.Error("a remote update that cannot be represented was applied")
	}
	if !errors.Is(err, ErrNotRepresentable) {
		t.Errorf("reason = %v, want ErrNotRepresentable", err)
	}
	// Refused, not stale: the two mean opposite things and the activity feed,
	// the counters and the diagnostics all branch on which it was.
	if err == nil {
		t.Error("a refusal reported as a plain stale rejection")
	}
}

// Valid UTF-8 that happens to be unusual must still go through: the refusal is
// about bytes that cannot survive, not about bytes that look odd.
func TestRepresentableValuesAreStillAccepted(t *testing.T) {
	kv := NewKVStore("n1")
	for name, value := range map[string]string{
		"empty":        "",
		"ascii":        "plain",
		"accented":     "héllo",
		"emoji":        "🙂",
		"cjk":          "日本語",
		"control":      "\x00\x01\x02",
		"quotes":       `"quoted" and \backslashed\`,
		"newlines":     "one\ntwo\r\nthree",
		"tabs":         "a\tb",
		"replacement":  "�", // a real U+FFFD a caller meant to write
		"combining":    "é",
		"rtl":          "مرحبا",
		"nullterminat": "value\x00",
	} {
		u, err := kv.Write(name, value, WriteOptions{})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		got, ok := kv.Get(name)
		if !ok || got != value {
			t.Errorf("%s: stored %q, want %q", name, got, value)
		}
		// And it survives the wire byte for byte, which is the whole claim.
		encoded, err := json.Marshal(u)
		if err != nil {
			t.Errorf("%s: marshal: %v", name, err)
			continue
		}
		var back pb.KVUpdate
		if err := json.Unmarshal(encoded, &back); err != nil {
			t.Errorf("%s: unmarshal: %v", name, err)
			continue
		}
		if !reflect.DeepEqual(u, back) {
			t.Errorf("%s: round trip changed the update:\n  sent %+v\n  got  %+v", name, u, back)
		}
	}
}

// The property the refusal buys, stated as the test that used to fail: two
// replicas that accept the same write hold the same bytes.
func TestAcceptedWritesReplicateByteForByte(t *testing.T) {
	a := NewKVStore("A")
	b := NewKVStore("B")

	for _, value := range []string{"plain", "héllo", "🙂", "\x00\x01", strings.Repeat("ß", 100)} {
		u, err := a.Write("k", value, WriteOptions{})
		if err != nil {
			t.Fatalf("write %q: %v", value, err)
		}
		wire, _ := json.Marshal(u)
		var onWire pb.KVUpdate
		if err := json.Unmarshal(wire, &onWire); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !b.Apply(onWire) {
			t.Fatalf("peer refused %q", value)
		}
		av, _ := a.Get("k")
		bv, _ := b.Get("k")
		if av != bv {
			t.Errorf("replicas diverged on %q: % x vs % x", value, av, bv)
		}
		// And the thing that made the old divergence unrepairable: equal
		// versions. If the bytes ever differ here again, this is why nothing
		// notices.
		if !reflect.DeepEqual(a.Digest("", 16), b.Digest("", 16)) {
			t.Errorf("digests differ for %q, which at least means it is repairable", value)
		}
	}
}

// Persistence marshals the same way, so a store that only refuses on the wire
// would still destroy its own copy on the next restart.
func TestPersistedStateSurvivesARestartByteForByte(t *testing.T) {
	kv := NewKVStore("A")
	for _, value := range []string{"plain", "héllo", "\x00\x01\x02", "🙂"} {
		if _, err := kv.Write("k:"+value, value, WriteOptions{}); err != nil {
			t.Fatalf("write %q: %v", value, err)
		}
	}
	encoded, err := json.Marshal(kv.State())
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	var reloaded PersistState
	if err := json.Unmarshal(encoded, &reloaded); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	restarted := NewKVStore("A")
	restarted.LoadState(reloaded)

	for _, e := range kv.Entries() {
		got, ok := restarted.Get(e.Key)
		if !ok {
			t.Errorf("%q is missing after a restart", e.Key)
			continue
		}
		if got != e.Value {
			t.Errorf("%q changed across a restart: % x -> % x", e.Key, e.Value, got)
		}
	}
}
