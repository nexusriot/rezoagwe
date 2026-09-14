package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/rezoagwe/pkg/discovery/node"
	"github.com/nexusriot/rezoagwe/pkg/transport"
)

func newGateway(t *testing.T) (*node.Node, string) {
	t.Helper()
	net := transport.NewMemNet(1)
	n, err := node.New(node.Config{
		NodeAddr:          "10.0.0.1:3137",
		Nick:              "alice",
		Transport:         net.Node("10.0.0.1:3137"),
		GossipInterval:    time.Hour,
		HeartbeatInterval: time.Hour,
		EvictThreshold:    time.Hour,
		SweepInterval:     time.Hour,
	}, nil)
	if err != nil {
		t.Fatalf("new node: %v", err)
	}
	n.Start()
	t.Cleanup(n.Stop)

	srv, err := Serve(Config{Addr: "127.0.0.1:0"}, n)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	return n, srv.URL()
}

func do(t *testing.T, method, url, body string, headers map[string]string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func bodyOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(data)
}

func TestKVCrud(t *testing.T) {
	n, base := newGateway(t)

	resp := do(t, http.MethodPut, base+"/kv/colour", "blue", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT status = %d, want 204: %s", resp.StatusCode, bodyOf(t, resp))
	}
	resp.Body.Close()
	if v, ok := n.Get("colour"); !ok || v != "blue" {
		t.Fatalf("store after PUT = (%q, %v)", v, ok)
	}

	resp = do(t, http.MethodGet, base+"/kv/colour", "", nil)
	if got := bodyOf(t, resp); got != "blue" {
		t.Fatalf("GET body = %q, want blue", got)
	}

	resp = do(t, http.MethodDelete, base+"/kv/colour", "", nil)
	resp.Body.Close()
	if _, ok := n.Get("colour"); ok {
		t.Fatal("key survived DELETE")
	}

	resp = do(t, http.MethodGet, base+"/kv/colour", "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET of a deleted key = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// If-Match turns a PUT into a compare-and-swap; a mismatch has to be refused
// rather than silently overwrite a concurrent write.
func TestConditionalPut(t *testing.T) {
	n, base := newGateway(t)

	resp := do(t, http.MethodPut, base+"/kv/lock", "alice", map[string]string{"If-Match": "*"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("claim status = %d, want 204", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	resp.Body.Close()
	if etag == "" {
		t.Fatal("no ETag on a successful write")
	}

	resp = do(t, http.MethodPut, base+"/kv/lock", "bob", map[string]string{"If-Match": "*"})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("second claim = %d, want 412", resp.StatusCode)
	}
	resp.Body.Close()

	resp = do(t, http.MethodPut, base+"/kv/lock", "bob", map[string]string{"If-Match": etag})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("matched CAS = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()
	if v, _ := n.Get("lock"); v != "bob" {
		t.Fatalf("value = %q, want bob", v)
	}

	resp = do(t, http.MethodPut, base+"/kv/lock", "eve", map[string]string{"If-Match": "not-a-version"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed If-Match = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestPutWithTTL(t *testing.T) {
	n, base := newGateway(t)

	resp := do(t, http.MethodPut, base+"/kv/session", "token",
		map[string]string{"X-Rezoagwe-TTL": "60"})
	resp.Body.Close()

	e, ok := n.Entry("session")
	if !ok || e.ExpiresAt == 0 {
		t.Fatalf("entry = %+v, want an expiry", e)
	}

	resp = do(t, http.MethodGet, base+"/kv/session", "", nil)
	if resp.Header.Get("X-Rezoagwe-Expires") == "" {
		t.Fatal("no expiry header on a key with a TTL")
	}
	resp.Body.Close()

	resp = do(t, http.MethodPut, base+"/kv/x", "v", map[string]string{"X-Rezoagwe-TTL": "soon"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed TTL = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestListWithPrefix(t *testing.T) {
	n, base := newGateway(t)
	n.Set("app/one", "1", 0)
	n.Set("app/two", "2", 0)
	n.Set("other", "3", 0)

	resp := do(t, http.MethodGet, base+"/kv?prefix=app/", "", nil)
	var items []kvItem
	if err := json.Unmarshal([]byte(bodyOf(t, resp)), &items); err != nil {
		t.Fatalf("unmarshal listing: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("listing = %d items, want 2: %+v", len(items), items)
	}
	for _, it := range items {
		if !strings.HasPrefix(it.Key, "app/") {
			t.Fatalf("prefix filter leaked %q", it.Key)
		}
		if it.Version == "" {
			t.Fatalf("listing item has no version: %+v", it)
		}
	}
}

func TestHealthAndMetrics(t *testing.T) {
	n, base := newGateway(t)
	n.Set("k", "v", 0)

	resp := do(t, http.MethodGet, base+"/health", "", nil)
	var status node.Status
	if err := json.Unmarshal([]byte(bodyOf(t, resp)), &status); err != nil {
		t.Fatalf("unmarshal health: %v", err)
	}
	if status.Keys != 1 || status.Nick != "alice" {
		t.Fatalf("health = %+v", status)
	}

	resp = do(t, http.MethodGet, base+"/metrics", "", nil)
	body := bodyOf(t, resp)
	if !strings.Contains(body, "rezoagwe_kv_local_writes_total 1") {
		t.Fatalf("metrics missing the local write counter:\n%s", body)
	}
}

func TestHistoryEndpoint(t *testing.T) {
	n, base := newGateway(t)
	n.Set("k", "v1", 0)
	n.Set("k", "v2", 0)

	resp := do(t, http.MethodGet, base+"/history/k", "", nil)
	var items []historyItem
	if err := json.Unmarshal([]byte(bodyOf(t, resp)), &items); err != nil {
		t.Fatalf("unmarshal history: %v", err)
	}
	if len(items) != 2 || items[1].Value != "v2" || !items[1].Local {
		t.Fatalf("history = %+v", items)
	}
}

func TestExportImport(t *testing.T) {
	n, base := newGateway(t)
	n.Set("k", "v", 0)

	resp := do(t, http.MethodGet, base+"/export", "", nil)
	dump := bodyOf(t, resp)

	n.Delete("k")
	if _, ok := n.Get("k"); ok {
		t.Fatal("key not deleted")
	}

	// A plain merge must lose to the newer tombstone; seeding must win.
	resp = do(t, http.MethodPost, base+"/import", dump, nil)
	resp.Body.Close()
	if _, ok := n.Get("k"); ok {
		t.Fatal("merge import resurrected a newer delete")
	}

	resp = do(t, http.MethodPost, base+"/import?mode=seed", dump, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed import = %d", resp.StatusCode)
	}
	resp.Body.Close()
	if v, ok := n.Get("k"); !ok || v != "v" {
		t.Fatalf("seed import did not restore the key: (%q, %v)", v, ok)
	}
}

// The HTTP client gets the same vocabulary as the TUI, commands included.
func TestChatEndpointUnderstandsCommands(t *testing.T) {
	n, base := newGateway(t)

	resp := do(t, http.MethodPost, base+"/chat", "/set via-http yes", nil)
	resp.Body.Close()
	if v, ok := n.Get("via-http"); !ok || v != "yes" {
		t.Fatalf("command over HTTP did not run: (%q, %v)", v, ok)
	}

	resp = do(t, http.MethodPost, base+"/chat", "hello", nil)
	resp.Body.Close()

	resp = do(t, http.MethodGet, base+"/chat", "", nil)
	if !strings.Contains(bodyOf(t, resp), "hello") {
		t.Fatal("chat message not in the log")
	}
}

func TestPeersEndpoint(t *testing.T) {
	n, base := newGateway(t)
	n.Model.AddPeer("10.0.0.2:3137")
	n.Model.SetNick("10.0.0.2:3137", "bob")

	resp := do(t, http.MethodGet, base+"/peers", "", nil)
	var peers []node.PeerInfo
	if err := json.Unmarshal([]byte(bodyOf(t, resp)), &peers); err != nil {
		t.Fatalf("unmarshal peers: %v", err)
	}
	if len(peers) != 1 || peers[0].Nick != "bob" {
		t.Fatalf("peers = %+v", peers)
	}
}

// readAll drains a response body as a string.
func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
