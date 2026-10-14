// Package httpapi exposes a node over HTTP, which is what makes the store
// scriptable — and what makes the cluster testable end to end without driving
// a terminal UI.
package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nexusriot/rezoagwe/pkg/discovery/diagnostics"
	"github.com/nexusriot/rezoagwe/pkg/discovery/model"
	"github.com/nexusriot/rezoagwe/pkg/discovery/node"
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

// maxBody caps a request body: a PUT value or an import file.
const maxBody = 32 << 20

// readBody reads a request body, refusing one past maxBody rather than
// truncating it.
//
// io.LimitReader stops at the limit and reports no error, so a PUT one byte
// over used to store the first maxBody bytes and answer 204: the client is
// told its value was written, and what the cluster replicates is a prefix of
// it. A body that does not fit has to be refused, not silently shortened.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, http.StatusRequestEntityTooLarge, "body exceeds %d bytes", maxBody)
			return nil, false
		}
		writeErr(w, http.StatusBadRequest, "read body: %s", err)
		return nil, false
	}
	return body, true
}

// Config describes the gateway.
//
// The gateway is a full read/write control surface on a cluster that
// authenticates every packet on the wire, and for a long time it authenticated
// nothing at all: anything that could reach the port could rewrite the whole
// store through POST /import?mode=seed. Token and TLS exist so binding it to
// anything but loopback is a defensible thing to do.
type Config struct {
	Addr string
	// Token, when set, is required on every request as either
	// "Authorization: Bearer <token>" or "X-Rezoagwe-Token: <token>".
	Token string
	// TLSCert and TLSKey serve HTTPS. A token over plain HTTP is a token on the
	// wire, so the two belong together on any network worth authenticating on.
	TLSCert string
	TLSKey  string
	// ReadOnly refuses every mutating request, which is what makes a gateway
	// safe to expose to a dashboard or a scraper.
	ReadOnly bool
}

// Server wraps an http.Server bound to a node.
type Server struct {
	node *node.Node
	http *http.Server
	addr string
	cfg  Config
}

// Serve starts the gateway and returns immediately.
func Serve(cfg Config, n *node.Node) (*Server, error) {
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		return nil, fmt.Errorf("TLS needs both a certificate and a key")
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, err
	}
	s := &Server{node: n, addr: ln.Addr().String(), cfg: cfg}
	s.http = &http.Server{
		Handler:           s.mux(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if cfg.TLSCert != "" {
		go s.http.ServeTLS(ln, cfg.TLSCert, cfg.TLSKey)
	} else {
		go s.http.Serve(ln)
	}
	return s, nil
}

// TLS reports whether the gateway is serving HTTPS.
func (s *Server) TLS() bool { return s.cfg.TLSCert != "" }

// URL is the base address a client should use.
func (s *Server) URL() string {
	if s.TLS() {
		return "https://" + s.addr
	}
	return "http://" + s.addr
}

// Addr is the resolved listen address, useful when the caller asked for :0.
func (s *Server) Addr() string { return s.addr }

func (s *Server) Close() error { return s.http.Close() }

func (s *Server) mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /peers", s.handlePeers)
	mux.HandleFunc("GET /activity", s.handleActivity)
	mux.HandleFunc("GET /kv", s.handleList)
	mux.HandleFunc("GET /kv/{key...}", s.handleGet)
	mux.HandleFunc("PUT /kv/{key...}", s.handlePut)
	mux.HandleFunc("DELETE /kv/{key...}", s.handleDelete)
	mux.HandleFunc("GET /history/{key...}", s.handleHistory)
	mux.HandleFunc("GET /chat", s.handleChatGet)
	mux.HandleFunc("POST /chat", s.handleChatPost)
	mux.HandleFunc("GET /export", s.handleExport)
	mux.HandleFunc("POST /import", s.handleImport)
	mux.HandleFunc("POST /snapshot", s.handleSnapshot)
	mux.HandleFunc("POST /repair", s.handleRepair)
	mux.HandleFunc("GET /topology", s.handleTopology)
	mux.HandleFunc("GET /diagnostics", s.handleDiagnostics)
	mux.HandleFunc("GET /consistency", s.handleConsistency)
	return s.count(s.authenticate(s.guardWrites(mux)))
}

func (s *Server) count(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.node.Metrics.HTTPRequests.Add(1)
		h.ServeHTTP(w, r)
	})
}

// authenticate refuses every request without the token, including /health and
// /metrics. There is deliberately no exemption: an unauthenticated route is a
// route somebody eventually hangs something else off, and a liveness probe can
// carry a header as easily as a browser can.
func (s *Server) authenticate(h http.Handler) http.Handler {
	if s.cfg.Token == "" {
		return h
	}
	want := []byte(s.cfg.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Rezoagwe-Token")
		if got == "" {
			if after, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
				got = after
			}
		}
		// Constant time, so a wrong token cannot be found one byte at a time.
		if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="rezoagwe"`)
			writeErr(w, http.StatusUnauthorized, "missing or invalid token")
			return
		}
		h.ServeHTTP(w, r)
	})
}

// guardWrites refuses anything that would change the store or the cluster.
func (s *Server) guardWrites(h http.Handler) http.Handler {
	if !s.cfg.ReadOnly {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			h.ServeHTTP(w, r)
		default:
			writeErr(w, http.StatusForbidden, "gateway is read-only")
		}
	})
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, format string, args ...interface{}) {
	writeJSON(w, code, map[string]string{"error": fmt.Sprintf(format, args...)})
}

// formatVersion renders a version as an ETag-friendly "counter.node".
func formatVersion(v pb.Version) string {
	return strconv.FormatUint(v.Counter, 10) + "." + v.Node
}

// parseVersion reads the form produced by formatVersion. The empty string and
// "*" both mean "the key must not exist", matching the If-Match convention.
func parseVersion(s string) (pb.Version, error) {
	s = strings.Trim(s, `"`)
	if s == "" || s == "*" {
		return pb.Version{}, nil
	}
	counter, node, ok := strings.Cut(s, ".")
	if !ok {
		return pb.Version{}, fmt.Errorf("malformed version %q", s)
	}
	c, err := strconv.ParseUint(counter, 10, 64)
	if err != nil {
		return pb.Version{}, fmt.Errorf("malformed version %q", s)
	}
	return pb.Version{Counter: c, Node: node}, nil
}

// condition reads a conditional header, reporting whether the client sent it
// at all.
//
// http.Header.Get cannot tell "no If-Match" from "If-Match:" with an empty
// value, and the two mean opposite things here: no header is an unconditional
// write, while an empty value is a condition — parseVersion reads it as the
// zero version, i.e. "the key must not exist". Testing the value for
// emptiness, as this used to, silently turned the second into the first, so a
// client that built its header from the ETag of a key that did not exist yet
// got an unconditional overwrite from the one request it wrote specifically to
// avoid one. A malformed If-Match is already refused with 400; an empty one
// has to fail closed too, not open.
func condition(r *http.Request, name string) (string, bool) {
	values, ok := r.Header[textproto.CanonicalMIMEHeaderKey(name)]
	if !ok || len(values) == 0 {
		return "", false
	}
	return values[0], true
}

// etagMatches implements the If-None-Match list form, including "*".
//
// Quotes are trimmed from both sides. If-Match on this same gateway goes
// through parseVersion, which tolerates the bare form — and the version a
// client reads from GET /kv is unquoted JSON — so being strict on only one of
// the two conditional headers would be an inconsistency of ours, not the
// client's.
func etagMatches(header, etag string) bool {
	want := strings.Trim(etag, `"`)
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.Trim(strings.TrimSpace(candidate), `"`)
		if candidate == "*" || candidate == want {
			return true
		}
	}
	return false
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.node.Status())
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	io.WriteString(w, s.node.Metrics.Snapshot().Prometheus())
}

func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.node.Peers())
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.node.Activity())
}

// handleRepair makes this node's copy of the named keys authoritative.
//
// A write surface in the strongest sense: it overwrites those keys on every
// replica. The keys have to be named — or `?divergent=true` has to be passed,
// which names them from this node's own consistency check — because "repair
// everything" on the wrong node is how a cluster is overwritten with stale
// data, and nothing in the protocol can tell which replica was right.
func (s *Server) handleRepair(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("divergent") == "true" {
		consistency, repair := s.node.RepairDivergent()
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"consistency": consistency,
			"repair":      repair,
		})
		return
	}

	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body: %s", err)
		return
	}
	if len(req.Keys) == 0 {
		writeErr(w, http.StatusBadRequest,
			"name the keys to repair, or pass ?divergent=true to use this node's consistency check")
		return
	}
	writeJSON(w, http.StatusOK, s.node.Repair(req.Keys))
}

// handleSnapshot writes an out-of-band snapshot, for a backup taken before a
// risky change rather than on the timer.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	path, err := s.node.WriteSnapshot()
	if err != nil {
		writeErr(w, http.StatusConflict, "%s", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": path})
}

func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.node.Topology())
}

// handleDiagnostics answers with the findings by default and the whole snapshot
// on request, because the findings are what a person reads and the snapshot is
// what they paste into a bug report.
func (s *Server) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	snap := s.node.Diagnostics()
	now := time.Now()
	if r.URL.Query().Get("format") == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, diagnostics.Report(snap, now))
		return
	}
	checks := diagnostics.Checks(snap, now)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"worst":    diagnostics.Worst(checks),
		"checks":   checks,
		"snapshot": snap,
	})
}

// handleConsistency asks every peer what it holds and reports who disagrees.
// It dials peers, so it is slower than every other route here by design.
func (s *Server) handleConsistency(w http.ResponseWriter, r *http.Request) {
	report := s.node.CheckConsistency()
	code := http.StatusOK
	if !report.Converged {
		// A divergent cluster is not a failed request, but a script polling
		// this should be able to branch on the status alone.
		code = http.StatusConflict
	}
	writeJSON(w, code, report)
}

// kvItem is one key in a listing.
type kvItem struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Version   string `json:"version"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	if watch := r.URL.Query().Get("watch"); watch != "" && watch != "0" && watch != "false" {
		s.handleWatch(w, r, prefix)
		return
	}
	entries := s.node.Entries()
	out := make([]kvItem, 0, len(entries))
	for _, e := range entries {
		if prefix != "" && !strings.HasPrefix(e.Key, prefix) {
			continue
		}
		out = append(out, kvItem{
			Key:       e.Key,
			Value:     e.Value,
			Version:   formatVersion(e.Version),
			ExpiresAt: e.ExpiresAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// watchPing is how often an idle stream writes a comment. A watch on a quiet
// prefix is indistinguishable from a dead connection otherwise, and anything
// between the client and the node will eventually reclaim one.
const watchPing = 20 * time.Second

// handleWatch streams store mutations as server-sent events.
//
// The subscription is opened *before* the baseline is read, so a write that
// lands between the two is delivered rather than lost. That can re-send an
// entry the snapshot already carried; every event names its version, so a
// client discards the duplicate with a comparison it has to make anyway. The
// other order drops the write silently, which is the bug this endpoint exists
// to stop people writing by hand against a poll.
func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request, prefix string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	watcher := s.node.Watch(prefix)
	defer watcher.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Nothing here is a byte stream a proxy should buffer up for efficiency.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	entries := s.node.Entries()
	baseline := make([]kvItem, 0, len(entries))
	for _, e := range entries {
		if prefix != "" && !strings.HasPrefix(e.Key, prefix) {
			continue
		}
		baseline = append(baseline, kvItem{
			Key:       e.Key,
			Value:     e.Value,
			Version:   formatVersion(e.Version),
			ExpiresAt: e.ExpiresAt,
		})
	}
	if !writeEvent(w, "snapshot", baseline) {
		return
	}
	flusher.Flush()

	ping := time.NewTicker(watchPing)
	defer ping.Stop()
	ctx := r.Context()
	// Drops are reported on the stream itself. A client told nothing cannot
	// tell a key that did not change from one it was too slow to hear about.
	reported := uint64(0)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-watcher.C:
			if !open {
				return
			}
			if dropped := watcher.Dropped(); dropped > reported {
				reported = dropped
				if !writeEvent(w, "dropped", map[string]uint64{"events": dropped}) {
					return
				}
			}
			if !writeEvent(w, "kv", ev) {
				return
			}
			flusher.Flush()
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeEvent frames one server-sent event, reporting whether the client is
// still there.
func writeEvent(w http.ResponseWriter, name string, v interface{}) bool {
	payload, err := json.Marshal(v)
	if err != nil {
		return false
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, payload)
	return err == nil
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	e, ok := s.node.Entry(key)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such key: %s", key)
		return
	}
	etag := `"` + formatVersion(e.Version) + `"`
	w.Header().Set("ETag", etag)
	if e.ExpiresAt > 0 {
		w.Header().Set("X-Rezoagwe-Expires", strconv.FormatInt(e.ExpiresAt, 10))
	}
	// The version is already the ETag, so a poller can ask "has this changed?"
	// instead of re-fetching a value it holds.
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, e.Value)
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var ttl time.Duration
	if raw := r.Header.Get("X-Rezoagwe-TTL"); raw != "" {
		secs, err := strconv.Atoi(raw)
		if err != nil || secs < 0 {
			writeErr(w, http.StatusBadRequest, "malformed X-Rezoagwe-TTL: %q", raw)
			return
		}
		ttl = time.Duration(secs) * time.Second
	}

	// If-Match turns the write into a compare-and-swap. Without it the write is
	// unconditional, which is the behaviour a plain PUT should have.
	if match, sent := condition(r, "If-Match"); sent {
		expect, err := parseVersion(match)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "%s", err)
			return
		}
		u, err := s.node.CompareAndSet(key, string(body), ttl, expect)
		if err != nil {
			writeRefusal(w, key, err)
			return
		}
		w.Header().Set("ETag", `"`+formatVersion(u.Version)+`"`)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	u, err := s.node.Set(key, string(body), ttl)
	if err != nil {
		writeRefusal(w, key, err)
		return
	}
	w.Header().Set("ETag", `"`+formatVersion(u.Version)+`"`)
	w.WriteHeader(http.StatusNoContent)
}

// writeRefusal turns the reason a write did not land into a status a client
// can act on.
//
// All of these used to be a 204 with an ETag of the zero version: Set's result
// was discarded, so a value over the size limit, or a key past the key limit,
// was dropped and reported as written. A client has no way to notice that, and
// the three cases need different answers — retry with less, free some keys, or
// re-read and try again.
func writeRefusal(w http.ResponseWriter, key string, err error) {
	switch {
	case errors.Is(err, model.ErrVersionMismatch):
		writeErr(w, http.StatusPreconditionFailed, "version mismatch for %s", key)
	case errors.Is(err, model.ErrNotRepresentable):
		// 422, not 400: the request is well-formed, and the body is exactly
		// the bytes the client meant to send. What cannot be processed is
		// storing them — every replica would hold something else.
		writeErr(w, http.StatusUnprocessableEntity,
			"%s (key %q): bodies are JSON on the wire, and a byte that is not valid UTF-8 "+
				"becomes U+FFFD on every replica while this node keeps the original. "+
				"Encode binary values (base64) before writing them.", err, key)
	case errors.Is(err, model.ErrUnshippable):
		writeErr(w, http.StatusRequestEntityTooLarge,
			"value for %s is too large to replicate: at most %d bytes once encoded",
			key, pb.MaxReplicableValueBytes)
	case errors.Is(err, model.ErrTooLarge):
		writeErr(w, http.StatusRequestEntityTooLarge, "value for %s exceeds this node's size limit", key)
	case errors.Is(err, model.ErrTooManyKeys):
		writeErr(w, http.StatusInsufficientStorage, "store is at its key limit; %s was not written", key)
	default:
		writeErr(w, http.StatusInternalServerError, "write %s: %s", key, err)
	}
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if match, sent := condition(r, "If-Match"); sent {
		expect, err := parseVersion(match)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "%s", err)
			return
		}
		if _, err := s.node.CompareAndDelete(key, expect); err != nil {
			writeRefusal(w, key, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// The result is not discarded here for the same reason it is not discarded
	// on a PUT: a delete the store will not take is a delete that did not
	// happen, and answering 204 tells the client otherwise. A key that is not
	// valid UTF-8 is the case that reaches this — the tombstone would land on
	// a different key on every replica.
	if _, err := s.node.Delete(key); err != nil {
		writeRefusal(w, key, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type historyItem struct {
	Version string `json:"version"`
	Value   string `json:"value,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
	Local   bool   `json:"local"`
	At      int64  `json:"at"`
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	h := s.node.History(key)
	out := make([]historyItem, 0, len(h))
	for _, e := range h {
		out = append(out, historyItem{
			Version: formatVersion(e.Version),
			Value:   e.Value,
			Deleted: e.Deleted,
			Local:   e.Local,
			At:      e.At.Unix(),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleChatGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.node.Chat())
}

func (s *Server) handleChatPost(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		writeErr(w, http.StatusBadRequest, "empty message")
		return
	}
	// Chat crosses the wire as JSON too, so the same bytes are mangled the
	// same way (§5.9). This is the only path that can carry arbitrary ones:
	// every other front end feeds this from a keyboard, which is UTF-8 by
	// construction.
	if !utf8.ValidString(text) {
		writeErr(w, http.StatusUnprocessableEntity,
			"message is not valid UTF-8; it would reach every peer with the invalid bytes "+
				"replaced by U+FFFD while this node kept the original")
		return
	}
	// Submit, not SendChat: the HTTP client gets the same slash commands the
	// TUI has.
	s.node.Submit(text)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	data, err := s.node.Export()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "export: %s", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="rezoagwe-export.json"`)
	w.Write(data)
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	// mode=seed re-stamps every entry as a local write so it outranks whatever
	// the cluster holds; the default merges by version.
	//
	// An unrecognised mode is refused rather than quietly read as "merge": the
	// two modes differ by whether the file overwrites the whole cluster, so a
	// typo ("mode=SEED") used to answer 200 for a restore that never happened.
	mode := r.URL.Query().Get("mode")
	if mode != "" && mode != "merge" && mode != "seed" {
		writeErr(w, http.StatusBadRequest, "unknown import mode %q: want \"merge\" or \"seed\"", mode)
		return
	}
	seed := mode == "seed"
	applied, err := s.node.Import(data, seed)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "import: %s", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"applied": applied})
}
