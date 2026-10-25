package node

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"strconv"
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
	Cursor string

	mu      sync.Mutex
	dropped uint64
	closed  bool
}

// Watch subscribes to every mutation whose key has the given prefix; an empty
// prefix watches the whole store. The caller must Close the watcher.
const watchHistoryLimit = 2048
const watchHistoryBytes = 8 << 20

var ErrResnapshot = errors.New("watch cursor expired, belongs to another prefix, or predates this process; resnapshot required")

func watchScope(prefix string) string {
	sum := sha256.Sum256([]byte(prefix))
	return fmt.Sprintf("%x", sum[:8])
}
func (w *Watcher) Token(id string) string    { return id + ":" + watchScope(w.prefix) }
func (n *Node) Watch(prefix string) *Watcher { w, _, _ := n.WatchFrom(prefix, ""); return w }

// WatchFrom installs the live subscription and captures replay under one lock.
func (n *Node) WatchFrom(prefix, cursor string) (*Watcher, []model.KVEvent, error) {
	n.watchMu.Lock()
	defer n.watchMu.Unlock()
	if n.watchEpoch == "" {
		n.watchEpoch = uuid.NewString()
	}
	after := n.eventSeq
	if cursor != "" {
		parts := strings.Split(cursor, ":")
		if len(parts) != 3 || parts[0] != n.watchEpoch || parts[2] != watchScope(prefix) {
			return nil, nil, ErrResnapshot
		}
		seq, err := strconv.ParseUint(parts[1], 10, 64)
		oldest := n.eventSeq - uint64(len(n.watchHistory))
		if err != nil || seq < oldest || seq > n.eventSeq {
			return nil, nil, ErrResnapshot
		}
		after = seq
	}
	ch := make(chan model.KVEvent, watchBuffer)
	n.watchSeq++
	w := &Watcher{C: ch, ch: ch, n: n, prefix: prefix, id: n.watchSeq, Cursor: fmt.Sprintf("%s:%d:%s", n.watchEpoch, n.eventSeq, watchScope(prefix))}
	if n.watchers == nil {
		n.watchers = make(map[uint64]*Watcher)
	}
	n.watchers[w.id] = w
	replay := []model.KVEvent{}
	if cursor != "" {
		for _, ev := range n.watchHistory {
			parts := strings.Split(ev.ID, ":")
			seq, _ := strconv.ParseUint(parts[1], 10, 64)
			if seq > after && strings.HasPrefix(ev.Update.Key, prefix) {
				replay = append(replay, ev)
			}
		}
	}
	return w, replay, nil
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
	defer n.watchMu.Unlock()
	if n.watchEpoch == "" {
		n.watchEpoch = uuid.NewString()
	}
	for _, ev := range events {
		n.eventSeq++
		ev.ID = fmt.Sprintf("%s:%d", n.watchEpoch, n.eventSeq)
		size := len(ev.Update.Key) + len(ev.Update.Value) + 128
		n.watchHistory = append(n.watchHistory, ev)
		n.watchBytes += size
		for len(n.watchHistory) > watchHistoryLimit || n.watchBytes > watchHistoryBytes {
			old := n.watchHistory[0]
			n.watchBytes -= len(old.Update.Key) + len(old.Update.Value) + 128
			n.watchHistory[0] = model.KVEvent{}
			n.watchHistory = n.watchHistory[1:]
		}
		for _, w := range n.watchers {
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
