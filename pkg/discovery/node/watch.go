package node

import (
	"strings"
	"sync"

	"github.com/nexusriot/rezoagwe/pkg/discovery/model"
)

// watchBuffer is how far behind a watcher may fall before its events are
// dropped. Replication must not block on a consumer, so the queue is bounded
// and an overrun is counted rather than waited out.
const watchBuffer = 256

// Watcher is a live subscription to store mutations.
//
// Events arrive on C in the order the store accepted them. A watcher that
// stops reading loses events rather than slowing the node down — see
// Dropped, which is the only way to tell a quiet key from a missed one.
type Watcher struct {
	// C delivers mutations until Close. It is closed by Close, so a range
	// over it terminates.
	C <-chan model.KVEvent

	ch     chan model.KVEvent
	n      *Node
	id     uint64
	prefix string

	mu      sync.Mutex
	dropped uint64
	closed  bool
}

// Watch subscribes to every mutation whose key has the given prefix; an empty
// prefix watches the whole store. The caller must Close the watcher.
func (n *Node) Watch(prefix string) *Watcher {
	ch := make(chan model.KVEvent, watchBuffer)
	w := &Watcher{C: ch, ch: ch, n: n, prefix: prefix}
	n.watchMu.Lock()
	n.watchSeq++
	w.id = n.watchSeq
	if n.watchers == nil {
		n.watchers = make(map[uint64]*Watcher)
	}
	n.watchers[w.id] = w
	n.watchMu.Unlock()
	return w
}

// Close unsubscribes and closes C. It is safe to call more than once.
func (w *Watcher) Close() {
	w.n.watchMu.Lock()
	delete(w.n.watchers, w.id)
	w.n.watchMu.Unlock()

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.closed = true
	close(w.ch)
}

// Dropped is how many events this watcher was too slow to take. A consumer
// that reports a key's history has to say when it has holes in it.
func (w *Watcher) Dropped() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped
}

// offer queues one event, reporting whether it fit.
func (w *Watcher) offer(ev model.KVEvent) bool {
	if w.prefix != "" && !strings.HasPrefix(ev.Update.Key, w.prefix) {
		return true // not ours to carry; not a drop either
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return true
	}
	select {
	case w.ch <- ev:
		return true
	default:
		w.dropped++
		return false
	}
}

// publishMutations fans store events out to the watchers. It runs on whichever
// goroutine performed the mutation — a replication path — so it never blocks.
func (n *Node) publishMutations(events []model.KVEvent) {
	n.watchMu.Lock()
	if len(n.watchers) == 0 {
		n.watchMu.Unlock()
		return
	}
	targets := make([]*Watcher, 0, len(n.watchers))
	for _, w := range n.watchers {
		targets = append(targets, w)
	}
	n.watchMu.Unlock()

	for _, ev := range events {
		for _, w := range targets {
			if !w.offer(ev) {
				n.Metrics.WatchDropped.Add(1)
			}
		}
	}
}

// WatcherCount is how many subscriptions are open, which is what makes a leaked
// watcher visible before it is a memory problem.
func (n *Node) WatcherCount() int {
	n.watchMu.Lock()
	defer n.watchMu.Unlock()
	return len(n.watchers)
}
