// Package httpapi exposes a node over HTTP, which is what makes the store
// scriptable — and what makes the cluster testable end to end without driving
// a terminal UI.
package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nexusriot/rezoagwe/pkg/discovery/node"
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

// maxBody caps a request body: a PUT value or an import file.
const maxBody = 32 << 20

// Server wraps an http.Server bound to a node.
type Server struct {
	node *node.Node
	http *http.Server
	addr string
}

// Serve starts the gateway on addr and returns immediately.
func Serve(addr string, n *node.Node) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{node: n, addr: ln.Addr().String()}
	s.http = &http.Server{
		Handler:           s.mux(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go s.http.Serve(ln)
	return s, nil
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
	return s.count(mux)
}

func (s *Server) count(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.node.Metrics.HTTPRequests.Add(1)
		h.ServeHTTP(w, r)
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

// kvItem is one key in a listing.
type kvItem struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Version   string `json:"version"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
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

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	e, ok := s.node.Entry(key)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such key: %s", key)
		return
	}
	w.Header().Set("ETag", `"`+formatVersion(e.Version)+`"`)
	if e.ExpiresAt > 0 {
		w.Header().Set("X-Rezoagwe-Expires", strconv.FormatInt(e.ExpiresAt, 10))
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, e.Value)
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: %s", err)
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
	if match := r.Header.Get("If-Match"); match != "" {
		expect, err := parseVersion(match)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "%s", err)
			return
		}
		u, ok := s.node.CompareAndSet(key, string(body), ttl, expect)
		if !ok {
			writeErr(w, http.StatusPreconditionFailed, "version mismatch for %s", key)
			return
		}
		w.Header().Set("ETag", `"`+formatVersion(u.Version)+`"`)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	u := s.node.Set(key, string(body), ttl)
	w.Header().Set("ETag", `"`+formatVersion(u.Version)+`"`)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if match := r.Header.Get("If-Match"); match != "" {
		expect, err := parseVersion(match)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "%s", err)
			return
		}
		if _, ok := s.node.CompareAndDelete(key, expect); !ok {
			writeErr(w, http.StatusPreconditionFailed, "version mismatch for %s", key)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.node.Delete(key)
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
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: %s", err)
		return
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		writeErr(w, http.StatusBadRequest, "empty message")
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
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: %s", err)
		return
	}
	// mode=seed re-stamps every entry as a local write so it outranks whatever
	// the cluster holds; the default merges by version.
	seed := r.URL.Query().Get("mode") == "seed"
	applied, err := s.node.Import(data, seed)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "import: %s", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"applied": applied})
}
