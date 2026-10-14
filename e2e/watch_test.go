package e2e

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A watch is the one gateway route that holds a connection open, and the only
// thing here that a unit test cannot exercise honestly: in-process there is no
// chunked transfer, no proxy buffering and no half-closed socket. The
// properties worth asserting are that the stream starts with a usable baseline
// and that a write made on *another* node arrives on it.

type sseEvent struct {
	name string
	data string
}

// openWatch starts a watch stream against a node and returns a reader over it.
func openWatch(t *testing.T, n node, query string) (*bufio.Reader, func()) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, n.url+"/kv?watch=1"+query, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("watch %s: %v", n.name, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("watch %s: status %d", n.name, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		resp.Body.Close()
		t.Fatalf("watch %s: content-type %q", n.name, ct)
	}
	return bufio.NewReader(resp.Body), func() { resp.Body.Close() }
}

// nextEvent reads one event, failing if none arrives before the deadline.
func nextEvent(t *testing.T, br *bufio.Reader, within time.Duration) sseEvent {
	t.Helper()
	type result struct {
		ev  sseEvent
		err error
	}
	done := make(chan result, 1)
	go func() {
		var ev sseEvent
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				done <- result{err: err}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, ": "):
				// the keep-alive ping
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			case line == "" && ev.name != "":
				done <- result{ev: ev}
				return
			}
		}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("read event: %v", r.err)
		}
		return r.ev
	case <-time.After(within):
		t.Fatal("no event arrived")
	}
	return sseEvent{}
}

func TestWatchOpensWithTheStoreAsItStands(t *testing.T) {
	br, closeStream := openWatch(t, a, "")
	defer closeStream()

	ev := nextEvent(t, br, converge)
	if ev.name != "snapshot" {
		t.Fatalf("first event = %q, want snapshot", ev.name)
	}
	var baseline []struct {
		Key     string `json:"key"`
		Value   string `json:"value"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(ev.data), &baseline); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	// The suite seeded this before any test ran, so a baseline that does not
	// carry it is a baseline taken from nothing.
	for _, e := range baseline {
		if e.Key == preJoinKey {
			if e.Value != preJoinVal {
				t.Errorf("baseline holds %q = %q, want %q", e.Key, e.Value, preJoinVal)
			}
			if e.Version == "" {
				t.Error("baseline entry carries no version, so a client cannot deduplicate it")
			}
			return
		}
	}
	t.Errorf("the seeded key is missing from the baseline of %d entries", len(baseline))
}

// The property that matters: a write made on one process arrives on a stream
// held open against another. Nothing in-process can test this.
func TestWatchDeliversAWriteMadeOnAnotherNode(t *testing.T) {
	br, closeStream := openWatch(t, b, "&prefix=watch/")
	defer closeStream()

	if ev := nextEvent(t, br, converge); ev.name != "snapshot" {
		t.Fatalf("first event = %q, want snapshot", ev.name)
	}

	// Written on a, watched on b, replicated between them over real sockets.
	a.put(t, "watch/from-a", "arrived by replication")

	deadline := time.Now().Add(converge)
	for time.Now().Before(deadline) {
		ev := nextEvent(t, br, converge)
		if ev.name != "kv" {
			continue
		}
		var got struct {
			Source string `json:"source"`
			Update struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			} `json:"update"`
		}
		if err := json.Unmarshal([]byte(ev.data), &got); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		if got.Update.Key != "watch/from-a" {
			continue
		}
		if got.Update.Value != "arrived by replication" {
			t.Errorf("value = %q, want the written value", got.Update.Value)
		}
		if got.Source != "remote" {
			t.Errorf("source = %q, want remote: it was written on another node", got.Source)
		}
		return
	}
	t.Fatal("the write never arrived on the stream")
}

// A key outside the prefix must not appear, or a client filtering server-side
// is paying for a filter that does not filter.
func TestWatchPrefixExcludesOtherKeys(t *testing.T) {
	br, closeStream := openWatch(t, a, "&prefix=scoped/")
	defer closeStream()

	if ev := nextEvent(t, br, converge); ev.name != "snapshot" {
		t.Fatalf("first event = %q, want snapshot", ev.name)
	}

	a.put(t, "unscoped/ignored", "should not appear")
	a.put(t, "scoped/wanted", "should appear")

	ev := nextEvent(t, br, converge)
	if ev.name != "kv" {
		t.Fatalf("event = %q, want kv", ev.name)
	}
	if strings.Contains(ev.data, "unscoped/ignored") {
		t.Errorf("a key outside the prefix was delivered: %s", ev.data)
	}
	if !strings.Contains(ev.data, "scoped/wanted") {
		t.Errorf("the prefixed key was not the first event: %s", ev.data)
	}
}

// A disconnected client must release its subscription, or every curl against
// the gateway leaves one behind for the life of the process.
func TestWatchSubscriptionsAreReleased(t *testing.T) {
	before := a.metric(t, `rezoagwe_watchers`)

	br, closeStream := openWatch(t, a, "")
	nextEvent(t, br, converge) // the snapshot, so the handler is certainly running

	deadline := time.Now().Add(converge)
	for a.metric(t, `rezoagwe_watchers`) <= before && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if got := a.metric(t, `rezoagwe_watchers`); got != before+1 {
		t.Fatalf("watchers = %v, want %v while a stream is open", got, before+1)
	}

	closeStream()
	for time.Now().Before(deadline) {
		// A write is what makes the handler notice the client has gone.
		a.put(t, "watch/poke", time.Now().String())
		if a.metric(t, `rezoagwe_watchers`) == before {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("watchers = %v after the client disconnected, want %v",
		a.metric(t, `rezoagwe_watchers`), before)
}

// The timing histograms have to be real series by the time a cluster has been
// gossiping for a minute, or the diagnostics rule that reads them has nothing
// to read.
func TestGossipTimingsAreExported(t *testing.T) {
	deadline := time.Now().Add(settle)
	for time.Now().Before(deadline) {
		if a.metric(t, `rezoagwe_gossip_round_seconds_count`) > 0 {
			break
		}
		time.Sleep(time.Second)
	}
	if got := a.metric(t, `rezoagwe_gossip_round_seconds_count`); got == 0 {
		t.Fatal("no gossip rounds were timed")
	}
	// A healthy cluster's worst tick must be nowhere near the eviction
	// threshold; this is the rule that catches a node too busy to stay a
	// member, and it is only meaningful if it is quiet when things are fine.
	if late := a.metric(t, `rezoagwe_gossip_lateness_seconds_max`); late > 5 {
		t.Errorf("worst gossip lateness = %vs on a healthy cluster", late)
	}
}
