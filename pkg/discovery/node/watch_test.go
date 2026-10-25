package node

import (
	"errors"
	"testing"
	"time"

	"github.com/nexusriot/rezoagwe/pkg/discovery/model"
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
	"github.com/nexusriot/rezoagwe/pkg/transport"
)

// nextEvent takes one event or fails, so a missed notification is a named
// failure rather than a test that hangs until the package timeout.
func nextEvent(t *testing.T, w *Watcher) model.KVEvent {
	t.Helper()
	select {
	case ev, ok := <-w.C:
		if !ok {
			t.Fatal("watcher closed while waiting for an event")
		}
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("no watch event arrived")
	}
	return model.KVEvent{}
}

func TestWatchDeliversLocalWrites(t *testing.T) {
	net := transport.NewMemNet(1)
	n := newTestNode(t, net, "10.0.0.1:3137")

	w := n.Watch("")
	defer w.Close()

	if _, err := n.Set("colour", "blue", 0); err != nil {
		t.Fatalf("set: %v", err)
	}
	ev := nextEvent(t, w)
	if ev.Source != model.KVSourceLocal {
		t.Errorf("source = %q, want %q", ev.Source, model.KVSourceLocal)
	}
	if ev.Update.Key != "colour" || ev.Update.Value != "blue" {
		t.Errorf("event = %+v, want colour=blue", ev.Update)
	}
	if ev.Update.Version.Zero() {
		t.Error("event carries no version, so a client cannot deduplicate it")
	}
}

func TestWatchDeliversDeletesAndRemoteUpdates(t *testing.T) {
	net := transport.NewMemNet(1)
	a, b := newPairedNodes(t, net)

	w := b.Watch("")
	defer w.Close()

	if _, err := a.Set("colour", "blue", 0); err != nil {
		t.Fatalf("set: %v", err)
	}
	ev := nextEvent(t, w)
	if ev.Source != model.KVSourceRemote {
		t.Errorf("source = %q, want %q", ev.Source, model.KVSourceRemote)
	}
	if ev.Update.Key != "colour" {
		t.Errorf("key = %q, want colour", ev.Update.Key)
	}

	if _, err := a.Delete("colour"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	ev = nextEvent(t, w)
	if ev.Update.Action != pb.KVDelete {
		t.Errorf("delete arrived as %+v, want a tombstone", ev.Update)
	}
}

// Expiry replicates nothing — every node applies it independently — so a
// watcher that only saw messages would never learn a key had gone.
func TestWatchReportsExpiry(t *testing.T) {
	net := transport.NewMemNet(1)
	n := newTestNode(t, net, "10.0.0.1:3137")

	w := n.Watch("")
	defer w.Close()

	if _, err := n.Set("session", "token", time.Second); err != nil {
		t.Fatalf("set: %v", err)
	}
	if ev := nextEvent(t, w); ev.Source != model.KVSourceLocal {
		t.Fatalf("first event source = %q", ev.Source)
	}

	n.Model.Store.SetClock(func() time.Time { return time.Now().Add(time.Hour) })
	if swept := n.Model.Store.SweepExpired(); swept != 1 {
		t.Fatalf("swept %d keys, want 1", swept)
	}
	ev := nextEvent(t, w)
	if ev.Source != model.KVSourceExpiry {
		t.Errorf("source = %q, want %q", ev.Source, model.KVSourceExpiry)
	}
	if ev.Update.Action != pb.KVDelete {
		t.Errorf("expiry arrived as %+v, want a tombstone", ev.Update)
	}
}

func TestWatchPrefixFiltersOtherKeys(t *testing.T) {
	net := transport.NewMemNet(1)
	n := newTestNode(t, net, "10.0.0.1:3137")

	w := n.Watch("app/")
	defer w.Close()

	if _, err := n.Set("other/key", "no", 0); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := n.Set("app/key", "yes", 0); err != nil {
		t.Fatalf("set: %v", err)
	}
	ev := nextEvent(t, w)
	if ev.Update.Key != "app/key" {
		t.Fatalf("got %q, want the prefixed key only", ev.Update.Key)
	}
	// A filtered key is not a drop: it was never this watcher's to carry.
	if d := w.Dropped(); d != 0 {
		t.Errorf("dropped = %d, want 0", d)
	}
}

func TestWatchCloseEndsTheStream(t *testing.T) {
	net := transport.NewMemNet(1)
	n := newTestNode(t, net, "10.0.0.1:3137")

	w := n.Watch("")
	if n.WatcherCount() != 1 {
		t.Fatalf("watcher count = %d, want 1", n.WatcherCount())
	}
	w.Close()
	if n.WatcherCount() != 0 {
		t.Fatalf("watcher count after close = %d, want 0", n.WatcherCount())
	}
	if _, open := <-w.C; open {
		t.Error("C still open after Close; a range over it would never end")
	}
	// Idempotent: a handler that defers Close and also closes on error must not
	// panic on the second call.
	w.Close()
	// And the node keeps working with nobody listening.
	if _, err := n.Set("colour", "blue", 0); err != nil {
		t.Fatalf("set after close: %v", err)
	}
}

// A watcher that stops reading must lose events rather than wedge the
// replication path that publishes them.
func TestWatchDropsRatherThanBlocks(t *testing.T) {
	net := transport.NewMemNet(1)
	n := newTestNode(t, net, "10.0.0.1:3137")

	w := n.Watch("")
	defer w.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < watchBuffer*2; i++ {
			n.Set("k", "v", 0)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("writes blocked on a watcher that never read")
	}
	if w.Dropped() == 0 {
		t.Error("nothing was dropped, so the overrun went unreported")
	}
	if got := n.Metrics.WatchDropped.Load(); got == 0 {
		t.Error("drops are invisible in the metrics")
	}
}

func TestWatchResumeScopesAndExpiry(t *testing.T) {
	n := newTestNode(t, transport.NewMemNet(1), "10.0.0.1:3137")
	w := n.Watch("a")
	cursor := w.Cursor
	w.Close()
	n.Set("a1", "one", 0)
	n.Set("b1", "skip", 0)
	w, replay, err := n.WatchFrom("a", cursor)
	if err != nil || len(replay) != 1 || replay[0].Update.Value != "one" {
		t.Fatal(replay, err)
	}
	cursor = w.Token(replay[0].ID)
	w.Close()
	if _, _, err = n.WatchFrom("b", cursor); !errors.Is(err, ErrResnapshot) {
		t.Fatal("prefix scope accepted")
	}
	for i := 0; i < watchHistoryLimit+1; i++ {
		n.Set("a1", "v", 0)
	}
	if _, _, err = n.WatchFrom("a", cursor); !errors.Is(err, ErrResnapshot) {
		t.Fatal("expired cursor accepted")
	}
}
