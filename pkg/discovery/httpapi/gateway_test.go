package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/rezoagwe/pkg/discovery/node"
	"github.com/nexusriot/rezoagwe/pkg/transport"
)

// guarded builds a gateway with an explicit config, which the default harness
// does not expose.
func guarded(t *testing.T, cfg Config) (*node.Node, string) {
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
	}, nil)
	if err != nil {
		t.Fatalf("new node: %v", err)
	}
	n.Start()
	t.Cleanup(n.Stop)

	cfg.Addr = "127.0.0.1:0"
	srv, err := Serve(cfg, n)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	return n, srv.URL()
}

// The hole this closed: every route was a full read/write control surface on
// the cluster, reachable by anything that could open the port.
func TestATokenIsRequiredOnEveryRoute(t *testing.T) {
	_, base := guarded(t, Config{Token: "s3cret"})

	for _, path := range []string{"/health", "/metrics", "/kv", "/peers", "/export",
		"/topology", "/diagnostics"} {
		resp := do(t, http.MethodGet, base+path, "", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without a token = %d, want 401", path, resp.StatusCode)
		}
		if resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("GET %s did not challenge", path)
		}
	}
	// Import is the one that rewrites the whole cluster.
	resp := do(t, http.MethodPost, base+"/import?mode=seed", `{"entries":[]}`, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated seed import = %d, want 401", resp.StatusCode)
	}
}

func TestTheTokenIsAcceptedInEitherHeader(t *testing.T) {
	_, base := guarded(t, Config{Token: "s3cret"})

	for name, headers := range map[string]map[string]string{
		"bearer": {"Authorization": "Bearer s3cret"},
		"custom": {"X-Rezoagwe-Token": "s3cret"},
	} {
		resp := do(t, http.MethodGet, base+"/health", "", headers)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s header = %d, want 200", name, resp.StatusCode)
		}
	}
	resp := do(t, http.MethodGet, base+"/health", "", map[string]string{"Authorization": "Bearer wrong"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a wrong token = %d, want 401", resp.StatusCode)
	}
}

func TestNoTokenConfiguredLeavesTheGatewayOpen(t *testing.T) {
	_, base := guarded(t, Config{})
	resp := do(t, http.MethodGet, base+"/health", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health without a configured token = %d, want 200", resp.StatusCode)
	}
}

func TestReadOnlyRefusesEveryMutation(t *testing.T) {
	n, base := guarded(t, Config{ReadOnly: true})
	n.Set("kept", "value", 0)

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPut, "/kv/x", "v"},
		{http.MethodDelete, "/kv/kept", ""},
		{http.MethodPost, "/chat", "hello"},
		{http.MethodPost, "/import", `{"entries":[]}`},
	} {
		resp := do(t, tc.method, base+tc.path, tc.body, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403", tc.method, tc.path, resp.StatusCode)
		}
	}
	if _, ok := n.Get("kept"); !ok {
		t.Fatal("a read-only gateway deleted a key")
	}
	resp := do(t, http.MethodGet, base+"/kv/kept", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read-only gateway refused a read: %d", resp.StatusCode)
	}
}

func TestTLSNeedsBothHalves(t *testing.T) {
	if _, err := Serve(Config{Addr: "127.0.0.1:0", TLSCert: "cert.pem"}, nil); err == nil {
		t.Fatal("a certificate without a key was accepted")
	}
	if _, err := Serve(Config{Addr: "127.0.0.1:0", TLSKey: "key.pem"}, nil); err == nil {
		t.Fatal("a key without a certificate was accepted")
	}
}

// The ETag was already published; honouring it is what stops a poller
// re-downloading a value it holds.
func TestConditionalGetReturnsNotModified(t *testing.T) {
	n, base := guarded(t, Config{})
	n.Set("k", "v", 0)

	first := do(t, http.MethodGet, base+"/kv/k", "", nil)
	first.Body.Close()
	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on a read")
	}

	again := do(t, http.MethodGet, base+"/kv/k", "", map[string]string{"If-None-Match": etag})
	again.Body.Close()
	if again.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional GET = %d, want 304", again.StatusCode)
	}

	n.Set("k", "v2", 0)
	changed := do(t, http.MethodGet, base+"/kv/k", "", map[string]string{"If-None-Match": etag})
	changed.Body.Close()
	if changed.StatusCode != http.StatusOK {
		t.Fatalf("GET after a write = %d, want 200", changed.StatusCode)
	}
}

// If-Match on this gateway tolerates the bare version, and GET /kv hands it out
// unquoted, so a client that sends it back unquoted is following our own API.
// Being strict on only one of the two conditional headers was our bug, not the
// client's — and it took the containerised suite to find it.
func TestConditionalGetAcceptsAnUnquotedVersion(t *testing.T) {
	n, base := guarded(t, Config{})
	n.Set("k", "v", 0)

	first := do(t, http.MethodGet, base+"/kv/k", "", nil)
	first.Body.Close()
	bare := strings.Trim(first.Header.Get("ETag"), `"`)

	for name, value := range map[string]string{
		"bare":      bare,
		"quoted":    `"` + bare + `"`,
		"wildcard":  "*",
		"in a list": `"nope", ` + bare,
	} {
		resp := do(t, http.MethodGet, base+"/kv/k", "", map[string]string{"If-None-Match": value})
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotModified {
			t.Errorf("If-None-Match %s (%q) = %d, want 304", name, value, resp.StatusCode)
		}
	}

	stale := do(t, http.MethodGet, base+"/kv/k", "", map[string]string{"If-None-Match": "1.other"})
	stale.Body.Close()
	if stale.StatusCode != http.StatusOK {
		t.Fatalf("a non-matching tag = %d, want 200", stale.StatusCode)
	}
}

func TestTopologyEndpointDescribesThisNode(t *testing.T) {
	_, base := guarded(t, Config{})
	resp := do(t, http.MethodGet, base+"/topology", "", nil)
	defer resp.Body.Close()

	var g struct {
		Nodes []struct {
			Addr string `json:"addr"`
			Role string `json:"role"`
		} `json:"nodes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(g.Nodes) != 1 || g.Nodes[0].Role != "self" {
		t.Fatalf("lone node topology = %+v", g.Nodes)
	}
}

func TestDiagnosticsEndpointServesChecksAndText(t *testing.T) {
	_, base := guarded(t, Config{})

	resp := do(t, http.MethodGet, base+"/diagnostics", "", nil)
	var payload struct {
		Worst  string `json:"worst"`
		Checks []struct {
			Severity string `json:"severity"`
			Title    string `json:"title"`
		} `json:"checks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if len(payload.Checks) == 0 || payload.Worst == "" {
		t.Fatalf("diagnostics returned nothing: %+v", payload)
	}

	text := do(t, http.MethodGet, base+"/diagnostics?format=text", "", nil)
	defer text.Body.Close()
	body := readAll(t, text)
	if !strings.Contains(body, "rezoagwe node diagnostics") {
		t.Fatalf("text report = %q", body)
	}
}

// A lone node is trivially converged: there is nobody to disagree with.
func TestConsistencyEndpointOnALoneNode(t *testing.T) {
	_, base := guarded(t, Config{})
	resp := do(t, http.MethodGet, base+"/consistency", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("consistency on a lone node = %d, want 200", resp.StatusCode)
	}
	var report struct {
		Converged bool       `json:"converged"`
		Peers     []struct{} `json:"peers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !report.Converged || len(report.Peers) != 0 {
		t.Fatalf("report = %+v", report)
	}
}

// Gauges are what an alert is written against, and /metrics carried none.
func TestMetricsExposesGauges(t *testing.T) {
	n, base := guarded(t, Config{})
	n.Set("a", "12345", 0)
	n.Set("b", "1", 0)
	n.Delete("b")

	resp := do(t, http.MethodGet, base+"/metrics", "", nil)
	defer resp.Body.Close()
	body := readAll(t, resp)

	for _, want := range []string{
		"# TYPE rezoagwe_keys gauge",
		"rezoagwe_keys 1",
		"rezoagwe_tombstones 1",
		"rezoagwe_value_bytes 5",
		"# TYPE rezoagwe_peers gauge",
		"# TYPE rezoagwe_lamport_clock gauge",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}
