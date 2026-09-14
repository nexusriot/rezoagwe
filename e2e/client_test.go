package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// node is one member of the cluster, addressed the way any client would address
// it: over the HTTP gateway, with no access to its internals.
type node struct {
	name string
	url  string
}

var (
	a       = node{"node-a", env("E2E_NODE_A", "http://node-a:8080")}
	b       = node{"node-b", env("E2E_NODE_B", "http://node-b:8080")}
	c       = node{"node-c", env("E2E_NODE_C", "http://node-c:8080")}
	late    = node{"node-late", env("E2E_NODE_LATE", "http://node-late:8080")}
	leaver  = node{"node-leaver", env("E2E_NODE_LEAVER", "http://node-leaver:8080")}
	restart = node{"node-restart", env("E2E_NODE_RESTART", "http://node-restart:8080")}
	wrongPS = node{"stranger-key", env("E2E_STRANGER_KEY", "http://stranger-key:8080")}
	wrongCl = node{"stranger-cluster", env("E2E_STRANGER_CLUSTER", "http://stranger-cluster:8080")}

	// The whole cluster as the suite expects it to settle, leaver excluded: it
	// is gone by the end and the tests that care name it explicitly.
	cluster = []node{a, b, c, late, restart}
	// The three that are up from the first second, which is what most
	// convergence assertions are about.
	core = []node{a, b, c}
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envSeconds(key string, fallback int) time.Duration {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return time.Duration(v) * time.Second
	}
	return time.Duration(fallback) * time.Second
}

var client = &http.Client{Timeout: 10 * time.Second}

// ---- the gateway, as a client sees it -------------------------------------

type entry struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Version   string `json:"version"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

type peer struct {
	Addr     string `json:"addr"`
	Nick     string `json:"nick"`
	LastSeen int64  `json:"last_seen"`
	AgeSec   int64  `json:"age_sec"`
}

type health struct {
	Addr       string `json:"addr"`
	Nick       string `json:"nick"`
	NodeID     string `json:"node_id"`
	Cluster    string `json:"cluster"`
	Peers      int    `json:"peers"`
	Keys       int    `json:"keys"`
	Tombstones int    `json:"tombstones"`
	UptimeSec  int64  `json:"uptime_sec"`
	Connected  bool   `json:"connected"`
}

type chatEntry struct {
	TS     int64  `json:"ts"`
	Sender string `json:"sender,omitempty"`
	Nick   string `json:"nick,omitempty"`
	Text   string `json:"text"`
	Kind   string `json:"kind,omitempty"`
	To     string `json:"to,omitempty"`
}

type graphNode struct {
	Addr       string `json:"addr"`
	Label      string `json:"label"`
	Role       string `json:"role"`
	Degree     int    `json:"degree"`
	Advertised int    `json:"advertised"`
}

type graphLink struct {
	A    string `json:"a"`
	B    string `json:"b"`
	Kind string `json:"kind"`
}

type graph struct {
	Nodes []graphNode `json:"nodes"`
	Links []graphLink `json:"links"`
}

type diagCheck struct {
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
}

type diagnostics struct {
	Worst  string      `json:"worst"`
	Checks []diagCheck `json:"checks"`
}

type peerConsistency struct {
	Addr             string `json:"addr"`
	Reachable        bool   `json:"reachable"`
	Error            string `json:"error,omitempty"`
	Keys             int    `json:"keys"`
	Agrees           bool   `json:"agrees"`
	DifferingBuckets []int  `json:"differing_buckets,omitempty"`
}

type consistency struct {
	Addr        string            `json:"addr"`
	Keys        int               `json:"keys"`
	Converged   bool              `json:"converged"`
	Unreachable int               `json:"unreachable"`
	Peers       []peerConsistency `json:"peers"`
}

type historyEntry struct {
	Version string `json:"version"`
	Value   string `json:"value,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
	Local   bool   `json:"local"`
	At      int64  `json:"at"`
}

func (n node) do(t *testing.T, method, path string, body string, headers map[string]string) (int, http.Header, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, n.url+path, rdr)
	if err != nil {
		t.Fatalf("%s: build %s %s: %v", n.name, method, path, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s: %s %s: %v", n.name, method, path, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: read %s %s: %v", n.name, method, path, err)
	}
	return resp.StatusCode, resp.Header, string(out)
}

func (n node) getJSON(t *testing.T, path string, into interface{}) {
	t.Helper()
	code, _, body := n.do(t, http.MethodGet, path, "", nil)
	if code != http.StatusOK {
		t.Fatalf("%s: GET %s = %d: %s", n.name, path, code, body)
	}
	if err := json.Unmarshal([]byte(body), into); err != nil {
		t.Fatalf("%s: GET %s returned unparseable JSON: %v\n%s", n.name, path, err, body)
	}
}

// ready is the readiness probe, and the one request that must not fail the test
// when it fails: a node the suite is waiting for is by definition not answering
// yet, and refusing the connection is how that looks.
func (n node) ready() error {
	resp, err := client.Get(n.url + "/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: /health = %s", n.name, resp.Status)
	}
	return nil
}

// waitReady blocks until the node answers, which for the timed containers is
// the difference between "not up yet" and "broken".
func waitReady(t *testing.T, n node, timeout time.Duration) {
	t.Helper()
	eventually(t, n.name+" to answer", timeout, func() (bool, string) {
		if err := n.ready(); err != nil {
			return false, err.Error()
		}
		return true, ""
	})
}

// waitRestarted blocks until the node's uptime goes backwards, which is the one
// unambiguous sign that the process behind the address was replaced. Waiting on
// a clock instead would mean agreeing with the container on when it started,
// and the suite and the container do not start together.
func waitRestarted(t *testing.T, n node, timeout time.Duration) {
	t.Helper()
	var highest int64 = -1
	eventually(t, n.name+" to go down and come back", timeout, func() (bool, string) {
		if err := n.ready(); err != nil {
			// Down: exactly what is expected in the middle of this.
			return false, err.Error()
		}
		up := n.health(t).UptimeSec
		if up < highest {
			return true, ""
		}
		if up > highest {
			highest = up
		}
		return false, fmt.Sprintf("still on the first process, up %ds", up)
	})
}

func (n node) health(t *testing.T) health {
	t.Helper()
	var h health
	n.getJSON(t, "/health", &h)
	return h
}

func (n node) peers(t *testing.T) []peer {
	t.Helper()
	var p []peer
	n.getJSON(t, "/peers", &p)
	return p
}

func (n node) peerAddrs(t *testing.T) []string {
	t.Helper()
	out := []string{}
	for _, p := range n.peers(t) {
		out = append(out, p.Addr)
	}
	return out
}

func (n node) entries(t *testing.T) []entry {
	t.Helper()
	var e []entry
	n.getJSON(t, "/kv", &e)
	return e
}

func (n node) chat(t *testing.T) []chatEntry {
	t.Helper()
	var e []chatEntry
	n.getJSON(t, "/chat", &e)
	return e
}

func (n node) history(t *testing.T, key string) []historyEntry {
	t.Helper()
	var h []historyEntry
	n.getJSON(t, "/history/"+key, &h)
	return h
}

// get returns the value, its version from the ETag, and whether the key exists.
func (n node) get(t *testing.T, key string) (string, string, bool) {
	t.Helper()
	code, hdr, body := n.do(t, http.MethodGet, "/kv/"+key, "", nil)
	switch code {
	case http.StatusOK:
		return body, strings.Trim(hdr.Get("ETag"), `"`), true
	case http.StatusNotFound:
		return "", "", false
	default:
		t.Fatalf("%s: GET /kv/%s = %d: %s", n.name, key, code, body)
		return "", "", false
	}
}

// put writes unconditionally and returns the new version.
func (n node) put(t *testing.T, key, value string) string {
	t.Helper()
	code, hdr, body := n.do(t, http.MethodPut, "/kv/"+key, value, nil)
	if code != http.StatusNoContent {
		t.Fatalf("%s: PUT /kv/%s = %d: %s", n.name, key, code, body)
	}
	return strings.Trim(hdr.Get("ETag"), `"`)
}

func (n node) putTTL(t *testing.T, key, value string, seconds int) string {
	t.Helper()
	hdrs := map[string]string{"X-Rezoagwe-TTL": strconv.Itoa(seconds)}
	code, hdr, body := n.do(t, http.MethodPut, "/kv/"+key, value, hdrs)
	if code != http.StatusNoContent {
		t.Fatalf("%s: PUT /kv/%s ttl=%d = %d: %s", n.name, key, seconds, code, body)
	}
	return strings.Trim(hdr.Get("ETag"), `"`)
}

// putIfMatch is the compare-and-swap form; it returns the status so a test can
// assert on the refusal as well as the success.
func (n node) putIfMatch(t *testing.T, key, value, expect string) (int, string) {
	t.Helper()
	hdrs := map[string]string{"If-Match": `"` + expect + `"`}
	code, hdr, _ := n.do(t, http.MethodPut, "/kv/"+key, value, hdrs)
	return code, strings.Trim(hdr.Get("ETag"), `"`)
}

func (n node) del(t *testing.T, key string) {
	t.Helper()
	code, _, body := n.do(t, http.MethodDelete, "/kv/"+key, "", nil)
	if code != http.StatusNoContent {
		t.Fatalf("%s: DELETE /kv/%s = %d: %s", n.name, key, code, body)
	}
}

func (n node) say(t *testing.T, text string) {
	t.Helper()
	code, _, body := n.do(t, http.MethodPost, "/chat", text, nil)
	if code != http.StatusNoContent {
		t.Fatalf("%s: POST /chat = %d: %s", n.name, code, body)
	}
}

func (n node) export(t *testing.T) string {
	t.Helper()
	code, _, body := n.do(t, http.MethodGet, "/export", "", nil)
	if code != http.StatusOK {
		t.Fatalf("%s: GET /export = %d: %s", n.name, code, body)
	}
	return body
}

func (n node) importState(t *testing.T, payload string) {
	t.Helper()
	code, _, body := n.do(t, http.MethodPost, "/import", payload, nil)
	if code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("%s: POST /import = %d: %s", n.name, code, body)
	}
}

// metric reads one counter out of the Prometheus exposition. Labelled series
// are addressed by their full `name{labels}` form.
func (n node) topology(t *testing.T) graph {
	t.Helper()
	var g graph
	n.getJSON(t, "/topology", &g)
	return g
}

func (n node) diagnostics(t *testing.T) diagnostics {
	t.Helper()
	var d diagnostics
	n.getJSON(t, "/diagnostics", &d)
	return d
}

// consistency accepts 409 as well as 200: a divergent cluster is a report, not
// a failed request.
func (n node) consistency(t *testing.T) consistency {
	t.Helper()
	code, _, body := n.do(t, http.MethodGet, "/consistency", "", nil)
	if code != http.StatusOK && code != http.StatusConflict {
		t.Fatalf("%s: GET /consistency = %d: %s", n.name, code, body)
	}
	var r consistency
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("%s: /consistency returned unparseable JSON: %v\n%s", n.name, err, body)
	}
	if (code == http.StatusOK) != r.Converged {
		t.Fatalf("%s: status %d disagrees with converged=%v", n.name, code, r.Converged)
	}
	return r
}

func (n node) metric(t *testing.T, series string) float64 {
	t.Helper()
	code, _, body := n.do(t, http.MethodGet, "/metrics", "", nil)
	if code != http.StatusOK {
		t.Fatalf("%s: GET /metrics = %d", n.name, code)
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, " ")
		if !ok || name != series {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			t.Fatalf("%s: metric %s is not a number: %q", n.name, series, value)
		}
		return v
	}
	return 0
}

// ---- waiting ---------------------------------------------------------------

// eventually polls until the condition holds, and fails with the last
// explanation the condition produced rather than a bare timeout — a test that
// says "timed out" and nothing else is a test you have to re-run by hand to
// learn anything from.
func eventually(t *testing.T, what string, timeout time.Duration, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var why string
	for time.Now().Before(deadline) {
		ok, detail := cond()
		if ok {
			return
		}
		why = detail
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s: %s", timeout, what, why)
}

// valueEverywhere waits until every listed node reports the same value for key.
func valueEverywhere(t *testing.T, nodes []node, key, want string, timeout time.Duration) {
	t.Helper()
	eventually(t, fmt.Sprintf("%q to read %q on every node", key, want), timeout, func() (bool, string) {
		for _, n := range nodes {
			got, _, ok := n.get(t, key)
			if !ok {
				return false, n.name + " does not have the key yet"
			}
			if got != want {
				return false, fmt.Sprintf("%s has %q", n.name, got)
			}
		}
		return true, ""
	})
}

func absentEverywhere(t *testing.T, nodes []node, key string, timeout time.Duration) {
	t.Helper()
	eventually(t, fmt.Sprintf("%q to be gone from every node", key), timeout, func() (bool, string) {
		for _, n := range nodes {
			if _, _, ok := n.get(t, key); ok {
				return false, n.name + " still has it"
			}
		}
		return true, ""
	})
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func chatTexts(entries []chatEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Text)
	}
	return out
}

func countText(entries []chatEntry, text string) int {
	n := 0
	for _, e := range entries {
		if e.Text == text {
			n++
		}
	}
	return n
}
