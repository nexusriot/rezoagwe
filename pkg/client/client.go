// Package client is a Go client for a node's HTTP gateway.
//
// It exists because looking at a cluster used to mean joining it. The TUI, the
// phone and the desktop app are all full peers: they gossip, they hold a
// replica, they appear in everyone's peer list. There was no way to ask a
// running node a question without becoming a member of its cluster, so the
// documented answer was curl with hand-written headers — and the conditional
// headers that make a compare-and-swap work are not something to write by hand
// twice.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nexusriot/rezoagwe/pkg/discovery/model"
	"github.com/nexusriot/rezoagwe/pkg/discovery/node"
)

// Client talks to one node's gateway.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// Config describes a client.
type Config struct {
	// BaseURL is the gateway root, e.g. "http://127.0.0.1:8080".
	BaseURL string
	// Token, when set, is sent as a bearer token on every request.
	Token string
	// Timeout bounds a request. Zero means 30 seconds — long enough for
	// /consistency, which dials every peer and is the slowest route by design.
	Timeout time.Duration
	// HTTPClient overrides the transport, for tests and for TLS settings.
	HTTPClient *http.Client
}

// New builds a client.
func New(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "http://127.0.0.1:8080"
	}
	if !strings.Contains(cfg.BaseURL, "://") {
		cfg.BaseURL = "http://" + cfg.BaseURL
	}
	hc := cfg.HTTPClient
	if hc == nil {
		timeout := cfg.Timeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		hc = &http.Client{Timeout: timeout}
	}
	return &Client{
		base:  strings.TrimSuffix(cfg.BaseURL, "/"),
		token: cfg.Token,
		http:  hc,
	}
}

// Error is a non-2xx answer from the gateway.
//
// The status is kept alongside the message because the gateway uses status
// codes to say things its body does not: 412 is a lost compare-and-swap, 409 a
// divergent cluster, 507 a store at its key limit. A caller that only reads the
// text cannot tell "someone got there first" from "this will never work".
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return http.StatusText(e.Status)
	}
	return e.Message
}

// Status reports the HTTP status behind an error, or 0 when it was not one the
// gateway produced (a dial failure, say).
func Status(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// NotFound reports whether an error is a 404, which is the one status a caller
// routinely wants to treat as an ordinary answer.
func NotFound(err error) bool { return Status(err) == http.StatusNotFound }

// Conflict reports whether an error is a failed precondition — a
// compare-and-swap that lost.
func Conflict(err error) bool { return Status(err) == http.StatusPreconditionFailed }

func (c *Client) request(method, path string, body io.Reader, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, readError(resp)
	}
	return resp, nil
}

// readError turns an error response into an Error, preferring the gateway's
// own message over the status text.
func readError(resp *http.Response) *Error {
	e := &Error{Status: resp.StatusCode}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil || len(data) == 0 {
		e.Message = http.StatusText(resp.StatusCode)
		return e
	}
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &payload) == nil && payload.Error != "" {
		e.Message = payload.Error
		return e
	}
	e.Message = strings.TrimSpace(string(data))
	return e
}

func (c *Client) getJSON(path string, out interface{}) error {
	resp, err := c.request(http.MethodGet, path, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

// Entry is a key as the gateway reports it.
type Entry struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Version   string `json:"version"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

// Get reads a key, returning its value and version.
func (c *Client) Get(key string) (value, version string, err error) {
	resp, err := c.request(http.MethodGet, "/kv/"+escapeKey(key), nil, nil)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	return string(data), strings.Trim(resp.Header.Get("ETag"), `"`), nil
}

// List returns every key with the given prefix; an empty prefix lists the
// whole store.
func (c *Client) List(prefix string) ([]Entry, error) {
	path := "/kv"
	if prefix != "" {
		path += "?prefix=" + queryEscape(prefix)
	}
	var out []Entry
	if err := c.getJSON(path, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// PutOptions modifies a write.
type PutOptions struct {
	// Durable requests an fsync acknowledgement; requires journal-sync on the server.
	Durable bool
	// TTL expires the key on every replica at the same instant.
	TTL time.Duration
	// IfMatch turns the write into a compare-and-swap against this version.
	// The literal "*" requires the key to be absent.
	IfMatch string
}

// Put writes a key and returns the version it landed at.
func (c *Client) Put(key, value string, opt PutOptions) (string, error) {
	headers := map[string]string{}
	if opt.Durable {
		headers["X-Rezoagwe-Durability"] = "durable"
	}
	if opt.TTL > 0 {
		headers["X-Rezoagwe-TTL"] = strconv.Itoa(int(opt.TTL.Seconds()))
	}
	if opt.IfMatch != "" {
		headers["If-Match"] = opt.IfMatch
	}
	resp, err := c.request(http.MethodPut, "/kv/"+escapeKey(key), strings.NewReader(value), headers)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	return strings.Trim(resp.Header.Get("ETag"), `"`), nil
}

// Delete removes a key. A non-empty ifMatch makes it a compare-and-delete.
func (c *Client) Delete(key, ifMatch string) error {
	headers := map[string]string{}
	if ifMatch != "" {
		headers["If-Match"] = ifMatch
	}
	resp, err := c.request(http.MethodDelete, "/kv/"+escapeKey(key), nil, headers)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Peers lists the node's peers.
func (c *Client) Peers() ([]node.PeerInfo, error) {
	var out []node.PeerInfo
	return out, c.getJSON("/peers", &out)
}

// Status is the node's own view of itself.
func (c *Client) Status() (node.Status, error) {
	var out node.Status
	return out, c.getJSON("/health", &out)
}

// History returns the recorded versions of a key.
func (c *Client) History(key string) ([]map[string]interface{}, error) {
	var out []map[string]interface{}
	return out, c.getJSON("/history/"+escapeKey(key), &out)
}

// Activity returns the replication activity feed.
func (c *Client) Activity() ([]string, error) {
	var out []string
	return out, c.getJSON("/activity", &out)
}

// Chat returns the conversation.
func (c *Client) Chat() ([]map[string]interface{}, error) {
	var out []map[string]interface{}
	return out, c.getJSON("/chat", &out)
}

// Say sends a chat line.
//
// The body is the text itself, not JSON around it: the gateway hands whatever
// arrives to the same submit path the TUI uses, so a line beginning with "/"
// is a slash command here exactly as it is there.
func (c *Client) Say(text string) error {
	resp, err := c.request(http.MethodPost, "/chat", strings.NewReader(text),
		map[string]string{"Content-Type": "text/plain; charset=utf-8"})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Doctor returns the diagnostics report as text.
func (c *Client) Doctor() (string, error) {
	resp, err := c.request(http.MethodGet, "/diagnostics?format=text", nil, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return string(data), err
}

// Topology returns the cluster graph.
func (c *Client) Topology() (map[string]interface{}, error) {
	var out map[string]interface{}
	return out, c.getJSON("/topology", &out)
}

// Consistency asks every peer what it holds.
//
// A divergent cluster answers 409, which is not a failed request: the report is
// the answer. Converged says which it was, so a caller does not have to decide
// from a status code whether it got one.
func (c *Client) Consistency() (report map[string]interface{}, converged bool, err error) {
	resp, err := c.request(http.MethodGet, "/consistency", nil, nil)
	if err != nil {
		if Status(err) == http.StatusConflict {
			// The body of a 409 is the report, and request consumed it.
			// Re-ask, reading it this time.
			return c.consistencyBody()
		}
		return nil, false, err
	}
	defer resp.Body.Close()
	err = json.NewDecoder(resp.Body).Decode(&report)
	return report, true, err
}

func (c *Client) consistencyBody() (map[string]interface{}, bool, error) {
	req, err := http.NewRequest(http.MethodGet, c.base+"/consistency", nil)
	if err != nil {
		return nil, false, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	var report map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		return nil, false, err
	}
	converged, _ := report["converged"].(bool)
	return report, converged, nil
}

// Export returns the whole store as an interchange file.
func (c *Client) Export() ([]byte, error) {
	resp, err := c.request(http.MethodGet, "/export", nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// Import loads an export. seed re-stamps every entry as a fresh local write so
// it wins; otherwise the exported versions are preserved and the data merges
// under the usual last-write-wins rule.
func (c *Client) Import(data []byte, seed bool) (int, error) {
	path := "/import"
	if seed {
		path += "?mode=seed"
	}
	resp, err := c.request(http.MethodPost, path, bytes.NewReader(data), nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var out struct {
		Applied int `json:"applied"`
	}
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out.Applied, err
}

// Repair makes this node's copy of the named keys authoritative across the
// cluster. See Node.Repair: run against the wrong replica it spreads the wrong
// value, which is why the keys are named rather than inferred.
func (c *Client) Repair(keys []string) (map[string]interface{}, error) {
	body, err := json.Marshal(map[string][]string{"keys": keys})
	if err != nil {
		return nil, err
	}
	resp, err := c.request(http.MethodPost, "/repair", bytes.NewReader(body),
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

// RepairDivergent runs the consistency check and repairs every key it names,
// which means "make this node's copy win wherever the replicas disagree".
func (c *Client) RepairDivergent() (map[string]interface{}, error) {
	resp, err := c.request(http.MethodPost, "/repair?divergent=true", nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

// Snapshot asks the node to write an out-of-band snapshot.
func (c *Client) Snapshot() (string, error) {
	resp, err := c.request(http.MethodPost, "/snapshot", nil, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Path string `json:"path"`
	}
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out.Path, err
}

// Metrics returns the Prometheus exposition text.
func (c *Client) Metrics() (string, error) {
	resp, err := c.request(http.MethodGet, "/metrics", nil, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return string(data), err
}

// WatchEvent is one event from a watch stream.
type WatchEvent struct {
	ID         string
	Resnapshot bool
	// Name is the server-sent event name: "snapshot" for the opening baseline,
	// "kv" for a mutation, "dropped" when the stream fell behind.
	Name string
	// Snapshot is set on the opening event.
	Snapshot []Entry
	// Event is set on a mutation.
	Event model.KVEvent
	// Dropped is how many events were lost, on a "dropped" event.
	Dropped uint64
}

// Watch streams store mutations until the context-free stop channel is closed
// or the connection ends. Events are delivered in order on the returned
// channel, which is closed when the stream ends; any error is sent last on
// errCh.
//
// The first event is always the baseline snapshot, taken *after* the
// subscription opened — so a write racing the call is delivered rather than
// lost, at the cost of possibly appearing twice. Every event names its version,
// which is what makes the duplicate recognisable.
func (c *Client) Watch(prefix string, stop <-chan struct{}) (<-chan WatchEvent, <-chan error) {
	return c.WatchFrom(prefix, "", stop)
}

// WatchFrom resumes from a previously delivered ID. HTTP 409 requires a fresh Watch.
func (c *Client) WatchFrom(prefix, cursor string, stop <-chan struct{}) (<-chan WatchEvent, <-chan error) {
	events := make(chan WatchEvent)
	errCh := make(chan error, 1)

	go func() {
		defer close(events)
		defer close(errCh)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			select {
			case <-stop:
				cancel()
			case <-ctx.Done():
			}
		}()
		path := "/kv?watch=1"
		if prefix != "" {
			path += "&prefix=" + queryEscape(prefix)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
		if err != nil {
			errCh <- err
			return
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		req.Header.Set("Accept", "text/event-stream")
		if cursor != "" {
			req.Header.Set("Last-Event-ID", cursor)
		}

		// A watch has no deadline by nature; the client's own timeout would
		// end a healthy stream on a quiet prefix.
		streaming := &http.Client{Transport: c.http.Transport}
		resp, err := streaming.Do(req)
		if err != nil {
			errCh <- err
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			errCh <- readError(resp)
			return
		}

		if err := scanEvents(resp.Body, events, stop); err != nil {
			select {
			case <-stop:
			default:
				errCh <- err
			}
		}
	}()
	return events, errCh
}

// scanEvents parses the server-sent event stream.
func scanEvents(r io.Reader, out chan<- WatchEvent, stop <-chan struct{}) error {
	br := bufio.NewReader(r)
	var name, data, id string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, ":"):
			// A comment; the keep-alive ping is one.
		case strings.HasPrefix(line, "id: "):
			id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "":
			if name == "" {
				continue
			}
			ev, err := parseEvent(name, data)
			ev.ID = id
			name, data, id = "", "", ""
			if err != nil {
				continue // a shape this client does not know is not fatal
			}
			select {
			case out <- ev:
			case <-stop:
				return nil
			}
		}
	}
}

func parseEvent(name, data string) (WatchEvent, error) {
	ev := WatchEvent{Name: name}
	switch name {
	case "ready":
		return ev, nil
	case "resnapshot":
		ev.Resnapshot = true
		return ev, nil
	case "snapshot":
		return ev, json.Unmarshal([]byte(data), &ev.Snapshot)
	case "kv":
		return ev, json.Unmarshal([]byte(data), &ev.Event)
	case "dropped":
		var payload struct {
			Events uint64 `json:"events"`
		}
		err := json.Unmarshal([]byte(data), &payload)
		ev.Dropped = payload.Events
		return ev, err
	}
	return ev, fmt.Errorf("unknown event %q", name)
}

// escapeKey encodes a key for the path segment of a URL.
//
// Keys are arbitrary strings — a slash, a space and a question mark are all
// legal — and the gateway routes /kv/{key...}, which takes the rest of the path
// verbatim after unescaping. Encode slashes and dots too: ServeMux cleans
// literal repeated slashes and dot segments before routing, redirecting the
// request to a different key (or even the listing endpoint).
func escapeKey(key string) string {
	var b strings.Builder
	for i := 0; i < len(key); i++ {
		ch := key[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
			b.WriteByte(ch)
		case ch == '-' || ch == '_' || ch == '~':
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

// queryEscape encodes a query-string value.
func queryEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
			b.WriteByte(ch)
		case ch == '-' || ch == '_' || ch == '.' || ch == '~':
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

// PreviewRepair returns a page of values and versions without changing state.
func (c *Client) PreviewRepair(keys []string, divergent bool, offset, limit int) (node.RepairPlan, error) {
	body, err := json.Marshal(map[string]interface{}{"keys": keys, "divergent": divergent, "offset": offset, "limit": limit})
	if err != nil {
		return node.RepairPlan{}, err
	}
	resp, err := c.request(http.MethodPost, "/repair/preview", bytes.NewReader(body), map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return node.RepairPlan{}, err
	}
	defer resp.Body.Close()
	var out node.RepairPlan
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out, err
}
func (c *Client) ApplyRepairPlan(plan node.RepairPlan) (node.RepairReport, error) {
	body, err := json.Marshal(plan)
	if err != nil {
		return node.RepairReport{}, err
	}
	resp, err := c.request(http.MethodPost, "/repair/apply", bytes.NewReader(body), map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return node.RepairReport{}, err
	}
	defer resp.Body.Close()
	var out node.RepairReport
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out, err
}

func (c *Client) RepairPlanPage(id string, offset, limit int) (node.RepairPlan, error) {
	body, _ := json.Marshal(map[string]interface{}{"id": id, "offset": offset, "limit": limit})
	resp, err := c.request(http.MethodPost, "/repair/preview", bytes.NewReader(body), map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return node.RepairPlan{}, err
	}
	defer resp.Body.Close()
	var out node.RepairPlan
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out, err
}

// PreviewRestore inspects a backup without mutating the store.
func (c *Client) PreviewRestore(backup json.RawMessage, deleteMissing, allowCrossCluster bool) (node.RepairPlan, error) {
	body, err := json.Marshal(map[string]interface{}{"backup": backup, "delete_missing": deleteMissing, "allow_cross_cluster": allowCrossCluster})
	if err != nil {
		return node.RepairPlan{}, err
	}
	resp, err := c.request(http.MethodPost, "/restore/preview", bytes.NewReader(body), map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return node.RepairPlan{}, err
	}
	defer resp.Body.Close()
	var out node.RepairPlan
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out, err
}
func (c *Client) ApplyRestore(id string, keys []string) (node.RepairReport, error) {
	body, _ := json.Marshal(map[string]interface{}{"id": id, "keys": keys})
	resp, err := c.request(http.MethodPost, "/restore/apply", bytes.NewReader(body), map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return node.RepairReport{}, err
	}
	defer resp.Body.Close()
	var out node.RepairReport
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out, err
}
