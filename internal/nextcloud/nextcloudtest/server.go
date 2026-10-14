// Package nextcloudtest provides a fake Nextcloud News API server for tests.
package nextcloudtest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Credentials accepted by the fake server (HTTP Basic auth).
const (
	User        = "alice"
	AppPassword = "secret-app-pw"
)

// Prefix is the path of the News API v1-3 below the Nextcloud URL.
const Prefix = "/index.php/apps/news/api/v1-3/"

// Call is one request received by the fake server, with Path relative to
// Prefix.
type Call struct {
	Method, Path, Query, Body string
}

// Server is a fake News API. Routes are keyed by "METHOD path", for instance
// "GET items" or "POST items/3/star"; unknown routes answer 404.
type Server struct {
	*httptest.Server

	mu    sync.Mutex
	calls []Call
}

// NewServer starts a fake News API that requires the User / AppPassword
// credentials. It is closed when the test ends.
func NewServer(t *testing.T, routes map[string]http.HandlerFunc) *Server {
	t.Helper()
	s := &Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != User || p != AppPassword {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		b, _ := io.ReadAll(r.Body)
		path := strings.TrimPrefix(r.URL.Path, Prefix)
		s.mu.Lock()
		s.calls = append(s.calls, Call{r.Method, path, r.URL.RawQuery, string(b)})
		s.mu.Unlock()
		if h, ok := routes[r.Method+" "+path]; ok {
			h(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(s.Close)
	return s
}

// Calls returns a copy of the requests received so far.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// LastCall returns the most recent request, or a zero Call.
func (s *Server) LastCall() Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		return Call{}
	}
	return s.calls[len(s.calls)-1]
}

// WriteJSON writes v as a JSON response.
func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
