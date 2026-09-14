package node

import (
	"fmt"
	"testing"

	"github.com/nexusriot/rezoagwe/pkg/transport"
)

// The bug this guards: the digest advertised 256 keys while a repair round
// could carry 128, and the sender's cursor advanced by the whole digest — so
// the tail of every badly-diverged range waited for a full wrap of the
// keyspace before anyone looked at it again.
func TestOneRoundRepairsEverythingTheDigestIdentified(t *testing.T) {
	net := transport.NewMemNet(1)
	a, b := newPairedNodes(t, net)

	const keys = 300
	for i := 0; i < keys; i++ {
		b.Model.Store.Set(fmt.Sprintf("k%04d", i), "v")
	}

	// a is empty, so every key its digest covers has to come back in one round.
	before := a.Model.Store.Len()
	a.antiEntropyRound(b.Addr())
	waitFor(t, "the first repair round to land", func() bool {
		return a.Model.Store.Len() > before
	})

	covered := a.cfg.DigestBatch
	waitFor(t, "every key the digest covered", func() bool {
		return a.Model.Store.Len() >= covered
	})
	if got := a.Model.Store.Len(); got < covered {
		t.Fatalf("one round repaired %d keys of the %d it advertised for", got, covered)
	}
}

// The clamp is what makes the round above whole; a digest wider than the repair
// budget is the configuration that strands keys.
func TestDigestBatchIsClampedToTheRepairBudget(t *testing.T) {
	cfg := Config{DigestBatch: 4096, MaxPush: 128, MaxPull: 64}
	cfg.applyDefaults()
	if cfg.DigestBatch != 64 {
		t.Fatalf("DigestBatch = %d, want it clamped to the smaller budget (64)", cfg.DigestBatch)
	}

	roomy := Config{DigestBatch: 32, MaxPush: 128, MaxPull: 128}
	roomy.applyDefaults()
	if roomy.DigestBatch != 32 {
		t.Fatalf("a digest inside the budget was changed to %d", roomy.DigestBatch)
	}
}

// A shared cursor divided the keyspace among whichever peers the random target
// picked, so covering the whole store against any one peer took as many wraps
// as there were peers. Each peer now gets its own walk.
func TestEachPeerHasItsOwnCursor(t *testing.T) {
	net := transport.NewMemNet(1)
	a := newTestNode(t, net, "10.0.0.1:3137")
	b := newTestNode(t, net, "10.0.0.2:3137")
	c := newTestNode(t, net, "10.0.0.3:3137")
	a.Model.AddPeer(b.Addr())
	a.Model.AddPeer(c.Addr())

	for i := 0; i < a.cfg.DigestBatch*3; i++ {
		a.Model.Store.Set(fmt.Sprintf("k%05d", i), "v")
	}

	a.antiEntropyRound(b.Addr())
	a.antiEntropyRound(c.Addr())

	a.aeMu.Lock()
	toB, toC := a.aeCursors[b.Addr()], a.aeCursors[c.Addr()]
	a.aeMu.Unlock()

	if toB == "" || toC == "" {
		t.Fatalf("cursors not advanced: b=%q c=%q", toB, toC)
	}
	if toB != toC {
		t.Fatalf("the first round against each peer covered different ranges: b=%q c=%q", toB, toC)
	}

	// A second round against b alone must not move c's cursor.
	a.antiEntropyRound(b.Addr())
	a.aeMu.Lock()
	toB2, toC2 := a.aeCursors[b.Addr()], a.aeCursors[c.Addr()]
	a.aeMu.Unlock()

	if toB2 == toB {
		t.Fatal("b's cursor did not advance on its second round")
	}
	if toC2 != toC {
		t.Fatalf("c's cursor moved (%q → %q) because of a round with b", toC, toC2)
	}
}

// A departing peer must not leave its cursor behind, or a long-running node
// accumulates one per address it has ever spoken to.
func TestADepartedPeersCursorIsForgotten(t *testing.T) {
	net := transport.NewMemNet(1)
	a, b := newPairedNodes(t, net)
	for i := 0; i < a.cfg.DigestBatch*2; i++ {
		a.Model.Store.Set(fmt.Sprintf("k%05d", i), "v")
	}
	a.antiEntropyRound(b.Addr())

	a.aeMu.Lock()
	_, present := a.aeCursors[b.Addr()]
	a.aeMu.Unlock()
	if !present {
		t.Fatal("no cursor recorded for the peer")
	}

	a.forgetPeerCursor(b.Addr())
	a.aeMu.Lock()
	_, stillThere := a.aeCursors[b.Addr()]
	a.aeMu.Unlock()
	if stillThere {
		t.Fatal("the cursor survived the peer")
	}
}

// Reaching the end of the keyspace has to start the walk over, or a store that
// shrinks below one digest is never swept again.
func TestTheCursorWrapsAtTheEndOfTheKeyspace(t *testing.T) {
	net := transport.NewMemNet(1)
	a, b := newPairedNodes(t, net)
	a.Model.Store.Set("only", "v")

	a.antiEntropyRound(b.Addr())
	a.aeMu.Lock()
	_, present := a.aeCursors[b.Addr()]
	a.aeMu.Unlock()
	if present {
		t.Fatal("a digest that reached the end of the keyspace left the cursor set")
	}
}
