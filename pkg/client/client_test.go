package client

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/rezoagwe/pkg/discovery/httpapi"
	"github.com/nexusriot/rezoagwe/pkg/discovery/model"
	"github.com/nexusriot/rezoagwe/pkg/discovery/node"
	"github.com/nexusriot/rezoagwe/pkg/transport"
)

// gateway starts a real node behind a real gateway. The client is the one part
// of this repository whose whole job is to agree with that surface, so a mock
// would test nothing.
func gateway(t *testing.T, cfg httpapi.Config) (*node.Node, *Client) {
	t.Helper()
	mem := transport.NewMemNet(1)
	n, err := node.New(node.Config{
		NodeAddr:          "10.0.0.1:3137",
		Nick:              "alice",
		Transport:         mem.Node("10.0.0.1:3137"),
		GossipInterval:    time.Hour,
		HeartbeatInterval: time.Hour,
		EvictThreshold:    time.Hour,
		SweepInterval:     time.Hour,
		Snapshot:          node.SnapshotConfig{Dir: t.TempDir()},
	}, nil)
	if err != nil {
		t.Fatalf("new node: %v", err)
	}
	n.Start()
	t.Cleanup(n.Stop)

	cfg.Addr = "127.0.0.1:0"
	srv, err := httpapi.Serve(cfg, n)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	return n, New(Config{BaseURL: srv.URL(), Token: cfg.Token})
}

func TestPutGetDelete(t *testing.T) {
	_, c := gateway(t, httpapi.Config{})

	version, err := c.Put("colour", "blue", PutOptions{})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if version == "" {
		t.Error("put returned no version")
	}

	value, gotVersion, err := c.Get("colour")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if value != "blue" {
		t.Errorf("value = %q, want blue", value)
	}
	if gotVersion != version {
		t.Errorf("version = %q, want %q", gotVersion, version)
	}

	if err := c.Delete("colour", ""); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, _, err := c.Get("colour"); !NotFound(err) {
		t.Errorf("get after delete: %v, want a not-found", err)
	}
}

func TestKeysAreNotNormalizedAsURLPaths(t *testing.T) {
	n, c := gateway(t, httpapi.Config{})
	for _, key := range []string{"a//b", "a/../b", "./b", ".", "..", "/b", "a/./b"} {
		t.Run(key, func(t *testing.T) {
			if _, err := n.Set(key, "original", 0); err != nil {
				t.Fatal(err)
			}
			got, _, err := c.Get(key)
			if err != nil || got != "original" {
				t.Fatalf("get %q = %q, %v", key, got, err)
			}
			if _, err := c.Put(key, "updated", PutOptions{}); err != nil {
				t.Fatal(err)
			}
			if got, _ := n.Get(key); got != "updated" {
				t.Fatalf("wrong key updated: %q", got)
			}
			if history, err := c.History(key); err != nil || len(history) < 2 {
				t.Fatalf("history = %v, %v", history, err)
			}
			if err := c.Delete(key, ""); err != nil {
				t.Fatal(err)
			}
			if _, ok := n.Get(key); ok {
				t.Fatal("key survived delete")
			}
		})
	}
}

// A lost compare-and-swap has to be distinguishable from every other failure,
// or a caller retries what will never work.
func TestCompareAndSwap(t *testing.T) {
	_, c := gateway(t, httpapi.Config{})

	version, err := c.Put("lock", "mine", PutOptions{IfMatch: "*"})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	// A second claim against "absent" must lose.
	if _, err := c.Put("lock", "yours", PutOptions{IfMatch: "*"}); !Conflict(err) {
		t.Errorf("second claim: %v, want a conflict", err)
	}
	// And against the version actually held, it wins.
	if _, err := c.Put("lock", "yours", PutOptions{IfMatch: version}); err != nil {
		t.Errorf("swap against the held version: %v", err)
	}
	if Status(err) != 0 {
		t.Errorf("Status on a nil error = %d, want 0", Status(err))
	}
}

func TestTTLIsCarried(t *testing.T) {
	_, c := gateway(t, httpapi.Config{})
	if _, err := c.Put("session", "token", PutOptions{TTL: 90 * time.Second}); err != nil {
		t.Fatalf("put: %v", err)
	}
	entries, err := c.List("")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 || entries[0].ExpiresAt == 0 {
		t.Fatalf("entries = %+v, want one with an expiry", entries)
	}
}

// Keys are arbitrary strings. A space or a question mark in one must not
// change which request this is.
func TestAwkwardKeysSurviveTheURL(t *testing.T) {
	_, c := gateway(t, httpapi.Config{})
	for _, key := range []string{
		"with space", "with?query", "with#hash", "with%percent",
		"nested/path/key", "unicode-é", "with+plus", "with&amp",
	} {
		if _, err := c.Put(key, "v:"+key, PutOptions{}); err != nil {
			t.Fatalf("put %q: %v", key, err)
		}
		got, _, err := c.Get(key)
		if err != nil {
			t.Fatalf("get %q: %v", key, err)
		}
		if got != "v:"+key {
			t.Errorf("key %q round-tripped to value %q", key, got)
		}
	}
}

func TestListFiltersByPrefix(t *testing.T) {
	_, c := gateway(t, httpapi.Config{})
	for _, key := range []string{"app/one", "app/two", "other/three"} {
		if _, err := c.Put(key, "v", PutOptions{}); err != nil {
			t.Fatalf("put %q: %v", key, err)
		}
	}
	entries, err := c.List("app/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("listed %d entries, want 2: %+v", len(entries), entries)
	}
}

func TestPeersAndStatus(t *testing.T) {
	n, c := gateway(t, httpapi.Config{})
	status, err := c.Status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Nick != "alice" || status.NodeID != n.Model.NodeID {
		t.Errorf("status = %+v, want alice's own identity", status)
	}
	peers, err := c.Peers()
	if err != nil {
		t.Fatalf("peers: %v", err)
	}
	if len(peers) != 0 {
		t.Errorf("peers = %+v, want none on a lone node", peers)
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	n, c := gateway(t, httpapi.Config{})
	if _, err := c.Put("colour", "blue", PutOptions{}); err != nil {
		t.Fatalf("put: %v", err)
	}
	data, err := c.Export()
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if err := c.Delete("colour", ""); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// A plain import preserves versions, so the export is older than the
	// delete and correctly loses.
	if _, err := c.Import(data, false); err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, ok := n.Get("colour"); ok {
		t.Error("a restore overrode a newer delete")
	}
	// A seed re-stamps every entry, so it wins.
	if applied, err := c.Import(data, true); err != nil {
		t.Fatalf("seed: %v", err)
	} else if applied != 1 {
		t.Errorf("seed applied %d entries, want 1", applied)
	}
	if v, ok := n.Get("colour"); !ok || v != "blue" {
		t.Errorf("after a seed: %q (%v), want blue", v, ok)
	}
}

func TestDoctorAndConsistency(t *testing.T) {
	_, c := gateway(t, httpapi.Config{})
	report, err := c.Doctor()
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report == "" {
		t.Error("doctor returned nothing")
	}
	got, converged, err := c.Consistency()
	if err != nil {
		t.Fatalf("consistency: %v", err)
	}
	if !converged {
		t.Errorf("a lone node disagrees with itself: %+v", got)
	}
}

func TestSnapshot(t *testing.T) {
	_, c := gateway(t, httpapi.Config{})
	if _, err := c.Put("colour", "blue", PutOptions{}); err != nil {
		t.Fatalf("put: %v", err)
	}
	path, err := c.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if path == "" {
		t.Error("snapshot returned no path")
	}
}

func TestChat(t *testing.T) {
	_, c := gateway(t, httpapi.Config{})
	if err := c.Say("hello from the cli"); err != nil {
		t.Fatalf("say: %v", err)
	}
	log, err := c.Chat()
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	found := false
	for _, e := range log {
		if text, _ := e["text"].(string); text == "hello from the cli" {
			found = true
		}
	}
	if !found {
		t.Errorf("the line is not in the log: %+v", log)
	}
}

func TestTokenIsSentAndEnforced(t *testing.T) {
	_, c := gateway(t, httpapi.Config{Token: "s3cret"})
	if _, err := c.Put("colour", "blue", PutOptions{}); err != nil {
		t.Fatalf("put with a token: %v", err)
	}

	// And without it, every route refuses.
	anon := New(Config{BaseURL: c.base})
	if _, _, err := anon.Get("colour"); Status(err) != http.StatusUnauthorized {
		t.Errorf("anonymous get: %v, want 401", err)
	}
	if _, err := anon.Status(); Status(err) != http.StatusUnauthorized {
		t.Errorf("anonymous health: %v, want 401", err)
	}
}

func TestReadOnlyGatewayRefusesWrites(t *testing.T) {
	_, c := gateway(t, httpapi.Config{ReadOnly: true})
	if _, err := c.Put("colour", "blue", PutOptions{}); Status(err) != http.StatusForbidden {
		t.Errorf("put on a read-only gateway: %v, want 403", err)
	}
	if _, err := c.List(""); err != nil {
		t.Errorf("list on a read-only gateway: %v, want it allowed", err)
	}
}

// The gateway's own message has to survive, or a refusal says "Conflict" where
// it could have said which limit was hit.
func TestErrorCarriesTheGatewaysMessage(t *testing.T) {
	_, c := gateway(t, httpapi.Config{})
	_, _, err := c.Get("nothing-here")
	if err == nil {
		t.Fatal("reading a missing key succeeded")
	}
	if !NotFound(err) {
		t.Errorf("Status = %d, want 404", Status(err))
	}
	if err.Error() == "" || err.Error() == "Not Found" {
		t.Errorf("error = %q, want the gateway's own message", err)
	}
}

func TestWatchStreamsChanges(t *testing.T) {
	n, c := gateway(t, httpapi.Config{})
	if _, err := c.Put("existing", "value", PutOptions{}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	stop := make(chan struct{})
	defer close(stop)
	events, errCh := c.Watch("", stop)

	select {
	case ev := <-events:
		if ev.Name != "snapshot" {
			t.Fatalf("first event = %q, want snapshot", ev.Name)
		}
		if len(ev.Snapshot) != 1 || ev.Snapshot[0].Key != "existing" {
			t.Fatalf("snapshot = %+v, want the one key", ev.Snapshot)
		}
	case err := <-errCh:
		t.Fatalf("watch: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("no snapshot event")
	}

	if _, err := n.Set("colour", "blue", 0); err != nil {
		t.Fatalf("set: %v", err)
	}
	select {
	case ev := <-events:
		if ev.Name != "kv" {
			t.Fatalf("event = %q, want kv", ev.Name)
		}
		if ev.Event.Update.Key != "colour" || ev.Event.Update.Value != "blue" {
			t.Errorf("event = %+v, want colour=blue", ev.Event.Update)
		}
		if ev.Event.Source != model.KVSourceLocal {
			t.Errorf("source = %q, want local", ev.Event.Source)
		}
	case err := <-errCh:
		t.Fatalf("watch: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("no change event")
	}
}

// Closing the stop channel must end the stream and release the subscription on
// the node, or every CLI invocation leaks one.
func TestWatchStopsAndUnsubscribes(t *testing.T) {
	n, c := gateway(t, httpapi.Config{})
	stop := make(chan struct{})
	events, _ := c.Watch("", stop)
	<-events // the snapshot, so the stream is certainly open

	deadline := time.Now().Add(3 * time.Second)
	for n.WatcherCount() != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)

	for range events {
		// drain until closed
	}
	for n.WatcherCount() != 0 && time.Now().Before(deadline) {
		n.Set("poke", "v", 0)
		time.Sleep(10 * time.Millisecond)
	}
	if got := n.WatcherCount(); got != 0 {
		t.Errorf("watcher count after stop = %d, want 0", got)
	}
}

func TestBaseURLGetsAScheme(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:8080":    "http://127.0.0.1:8080",
		"http://host:8080":  "http://host:8080",
		"https://host:8080": "https://host:8080",
		"http://host:8080/": "http://host:8080",
	} {
		if got := New(Config{BaseURL: in}).base; got != want {
			t.Errorf("New(%q).base = %q, want %q", in, got, want)
		}
	}
}

func TestRepairThroughTheClient(t *testing.T) {
	n, c := gateway(t, httpapi.Config{})
	if _, err := n.Set("colour", "blue", 0); err != nil {
		t.Fatalf("set: %v", err)
	}
	before, _ := n.Entry("colour")

	out, err := c.Repair([]string{"colour"})
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if rewritten, _ := out["rewritten"].(float64); rewritten != 1 {
		t.Errorf("rewrote %v keys, want 1: %+v", out["rewritten"], out)
	}
	after, _ := n.Entry("colour")
	if after.Version.Counter <= before.Version.Counter {
		t.Error("the version did not advance")
	}
}

func TestRepairDivergentThroughTheClient(t *testing.T) {
	_, c := gateway(t, httpapi.Config{})
	out, err := c.RepairDivergent()
	if err != nil {
		t.Fatalf("repair -divergent: %v", err)
	}
	if _, ok := out["consistency"]; !ok {
		t.Errorf("no consistency section: %+v", out)
	}
}

// The refusal has to survive as a refusal, not become an empty report.
func TestRepairWithNoKeysReportsTheRefusal(t *testing.T) {
	_, c := gateway(t, httpapi.Config{})
	if _, err := c.Repair(nil); err == nil {
		t.Error("repairing nothing succeeded")
	} else if Status(err) != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", Status(err))
	}
}

func TestSharedCorpusURLKeys(t *testing.T) {
	_, c := gateway(t, httpapi.Config{})
	data, err := os.ReadFile("../../testdata/boundaries.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct{ Entries []struct{ Key, Value string } }
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	for _, e := range corpus.Entries {
		if _, err := c.Put(e.Key, e.Value, PutOptions{}); err != nil {
			t.Fatal(err)
		}
		got, _, err := c.Get(e.Key)
		if err != nil || got != e.Value {
			t.Fatalf("%q: %q %v", e.Key, got, err)
		}
	}
}

func TestRepairPreviewApplyAndStablePages(t *testing.T) {
	n, c := gateway(t, httpapi.Config{})
	for _, key := range []string{"a", "b", "c"} {
		n.Set(key, "original", 0)
	}
	p, err := c.PreviewRepair([]string{"a", "b", "c"}, false, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if p.Total != 3 || p.Next != 1 {
		t.Fatalf("preview %+v", p)
	}
	n.Set("a", "concurrent", 0)
	r, err := c.ApplyRepairPlan(p)
	if err != nil || r.Changed != 1 {
		t.Fatalf("apply %+v %v", r, err)
	}
	next, err := c.RepairPlanPage(p.ID, p.Next, 1)
	if err != nil || next.Items[0].Key != "b" {
		t.Fatalf("page shifted: %+v %v", next, err)
	}
}

func TestResumeWatchThroughGateway(t *testing.T) {
	n, c := gateway(t, httpapi.Config{})
	stop := make(chan struct{})
	events, errs := c.Watch("app/", stop)
	var first WatchEvent
	select {
	case first = <-events:
	case <-time.After(time.Second):
		t.Fatal("no snapshot")
	}
	if first.ID == "" || first.Name != "snapshot" {
		t.Fatal(first)
	}
	close(stop)
	for range events {
	}
	for range errs {
	}
	n.Set("app/key", "after disconnect", 0)
	stop = make(chan struct{})
	defer close(stop)
	events, _ = c.WatchFrom("app/", first.ID, stop)
	select {
	case ev := <-events:
		if ev.Name != "kv" || ev.ID == "" || ev.Event.Update.Value != "after disconnect" {
			t.Fatal(ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no replay")
	}
	select {
	case ev := <-events:
		if ev.Name != "ready" {
			t.Fatal(ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no ready event")
	}
}

func TestRestoreAPIAndPagedPreview(t *testing.T) {
	n, c := gateway(t, httpapi.Config{})
	n.Set("k", "old", 0)
	p, err := c.PreviewRestore(json.RawMessage(`{"cluster":"rezoagwe","entries":[{"key":"k","value":"new","action":"set"}]}`), false, false)
	if err != nil || p.Total != 1 {
		t.Fatal(p, err)
	}
	r, err := c.ApplyRestore(p.ID, []string{"k"})
	if err != nil || r.Rewritten != 1 {
		t.Fatal(r, err)
	}
	n.Set("large", strings.Repeat("€", 1000), 0)
	resp, err := c.request(http.MethodGet, "/kv?limit=1&offset=1&preview=true", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page struct {
		Entries []struct {
			Value     string
			Truncated bool
		}
		Total int
	}
	if err = json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Entries) != 1 || !page.Entries[0].Truncated || len(page.Entries[0].Value) > 512 {
		t.Fatal(page)
	}
}

func TestDurablePutAndRecovery(t *testing.T) {
	mem := transport.NewMemNet(1)
	file := filepath.Join(t.TempDir(), "state.json")
	n, err := node.New(node.Config{NodeAddr: "10.0.0.1:3137", Transport: mem.Node("10.0.0.1:3137"), DataPath: file, Durability: "journal-sync"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	n.Start()
	t.Cleanup(n.Stop)
	srv, err := httpapi.Serve(httpapi.Config{Addr: "127.0.0.1:0"}, n)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	c := New(Config{BaseURL: srv.URL()})
	if _, err = c.Put("durable", "saved", PutOptions{Durable: true}); err != nil {
		t.Fatal(err)
	}
	if !n.Model.PersistenceHealth().Durable {
		t.Fatal("durability not confirmed")
	}
	p := model.NewPersister(file)
	state, found, err := p.Load()
	if err != nil || !found || state.Clock == 0 {
		t.Fatal(state, found, err)
	}
	_, plain := gateway(t, httpapi.Config{})
	if _, err = plain.Put("no", "write", PutOptions{Durable: true}); err == nil {
		t.Fatal("accepted durable write without persistence")
	}
	if _, _, err = plain.Get("no"); err == nil {
		t.Fatal("refused write mutated data")
	}
}
