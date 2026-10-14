package httpapi

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/rezoagwe/pkg/discovery/model"
)

// sseEvent is one parsed server-sent event.
type sseEvent struct {
	Name string
	Data string
}

// readEvent reads one event or fails. SSE frames end at a blank line.
func readEvent(t *testing.T, br *bufio.Reader) sseEvent {
	t.Helper()
	var ev sseEvent
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read event: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "" && ev.Name != "":
			return ev
		case strings.HasPrefix(line, "event: "):
			ev.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			ev.Data = strings.TrimPrefix(line, "data: ")
		}
	}
}

// openWatch starts a watch stream and returns a reader over it.
func openWatch(t *testing.T, url string) *bufio.Reader {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("watch status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	return bufio.NewReader(resp.Body)
}

func TestWatchStreamsSnapshotThenChanges(t *testing.T) {
	n, url := newGateway(t)
	if _, err := n.Set("existing", "value", 0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	br := openWatch(t, url+"/kv?watch=1")

	snap := readEvent(t, br)
	if snap.Name != "snapshot" {
		t.Fatalf("first event = %q, want snapshot", snap.Name)
	}
	var baseline []kvItem
	if err := json.Unmarshal([]byte(snap.Data), &baseline); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if len(baseline) != 1 || baseline[0].Key != "existing" {
		t.Fatalf("snapshot = %+v, want the one existing key", baseline)
	}

	// The snapshot is the baseline; this write happens after it.
	if _, err := n.Set("colour", "blue", 0); err != nil {
		t.Fatalf("set: %v", err)
	}
	ev := readEvent(t, br)
	if ev.Name != "kv" {
		t.Fatalf("event = %q, want kv", ev.Name)
	}
	var got model.KVEvent
	if err := json.Unmarshal([]byte(ev.Data), &got); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if got.Update.Key != "colour" || got.Update.Value != "blue" {
		t.Errorf("event = %+v, want colour=blue", got.Update)
	}
	if got.Source != model.KVSourceLocal {
		t.Errorf("source = %q, want local", got.Source)
	}
}

func TestWatchHonoursPrefix(t *testing.T) {
	n, url := newGateway(t)
	if _, err := n.Set("other/seed", "x", 0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	br := openWatch(t, url+"/kv?watch=1&prefix=app/")

	snap := readEvent(t, br)
	var baseline []kvItem
	if err := json.Unmarshal([]byte(snap.Data), &baseline); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if len(baseline) != 0 {
		t.Fatalf("snapshot = %+v, want nothing outside the prefix", baseline)
	}

	if _, err := n.Set("other/key", "no", 0); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := n.Set("app/key", "yes", 0); err != nil {
		t.Fatalf("set: %v", err)
	}
	ev := readEvent(t, br)
	var got model.KVEvent
	if err := json.Unmarshal([]byte(ev.Data), &got); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if got.Update.Key != "app/key" {
		t.Errorf("key = %q, want the prefixed key only", got.Update.Key)
	}
}

// A disconnected client must not leave a subscription behind: the node would
// publish into it forever.
func TestWatchUnsubscribesOnDisconnect(t *testing.T) {
	n, url := newGateway(t)

	req, err := http.NewRequest(http.MethodGet, url+"/kv?watch=1", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	br := bufio.NewReader(resp.Body)
	readEvent(t, br) // the snapshot, so the handler is certainly running

	deadline := time.Now().Add(3 * time.Second)
	for n.WatcherCount() != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n.WatcherCount() != 1 {
		t.Fatalf("watcher count = %d, want 1", n.WatcherCount())
	}

	resp.Body.Close()
	for n.WatcherCount() != 0 && time.Now().Before(deadline) {
		n.Set("poke", "v", 0) // a write is what makes the handler notice
		time.Sleep(10 * time.Millisecond)
	}
	if got := n.WatcherCount(); got != 0 {
		t.Errorf("watcher count after disconnect = %d, want 0", got)
	}
}

// A watch is a read, so a read-only gateway must still serve it.
func TestWatchWorksOnAReadOnlyGateway(t *testing.T) {
	_, url := guarded(t, Config{ReadOnly: true})
	br := openWatch(t, url+"/kv?watch=1")
	if ev := readEvent(t, br); ev.Name != "snapshot" {
		t.Fatalf("first event = %q, want snapshot", ev.Name)
	}
}
