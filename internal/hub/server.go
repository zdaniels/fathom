package hub

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Server hosts the hub HTTP API + the static frontend at /. Bind to a
// separate port from the gateway (default 8791) so they're independently
// scalable.
type Server struct {
	host    string
	port    int
	dataDir string
	webDir  string
	server  *http.Server
	storage *Storage
}

// Options configures a Server.
type Options struct {
	Host    string
	Port    int
	DataDir string // where tarballs + metadata are persisted
	WebDir  string // optional — static frontend (hub-web/)
}

// New returns a server ready to Start.
func New(opts Options) (*Server, error) {
	host := opts.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := opts.Port
	if port == 0 {
		port = 8791
	}
	if opts.DataDir == "" {
		return nil, errors.New("hub: DataDir is required")
	}
	storage, err := NewStorage(opts.DataDir)
	if err != nil {
		return nil, err
	}
	return &Server{host: host, port: port, dataDir: opts.DataDir, webDir: opts.WebDir, storage: storage}, nil
}

// Storage returns the underlying store — used by start-local to pre-populate
// the hub with built-in skill metadata.
func (s *Server) Storage() *Storage { return s.storage }

// Start binds the listener and serves in a background goroutine.
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/hub/v1/skills", s.handleListOrSearch)
	mux.HandleFunc("/hub/v1/skills/", s.handleSkill) // /skills/{name}, /skills/{name}/download
	mux.HandleFunc("/hub/v1/stats", s.handleStats)
	mux.HandleFunc("/hub/v1/health", s.handleHealth)
	if s.webDir != "" {
		fs := http.FileServer(http.Dir(s.webDir))
		mux.Handle("/", fs)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(s.host, itoa(s.port)))
	if err != nil {
		return err
	}
	s.port = ln.Addr().(*net.TCPAddr).Port
	s.server = &http.Server{
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}
	go func() {
		if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("hub server error", "err", err)
		}
	}()
	slog.Info("Fathom Secure Hub listening", "host", s.host, "port", s.port)
	return nil
}

// Stop shuts the hub down.
func (s *Server) Stop(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}

func (s *Server) handleListOrSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errMsg("GET only"))
		return
	}
	q := strings.ToLower(r.URL.Query().Get("q"))
	all := s.storage.List()
	if q == "" {
		writeJSON(w, http.StatusOK, map[string]interface{}{"skills": all, "count": len(all)})
		return
	}
	hits := all[:0]
	for _, m := range all {
		if strings.Contains(strings.ToLower(m.Name), q) ||
			strings.Contains(strings.ToLower(m.Description), q) ||
			tagsMatch(m.Tags, q) {
			hits = append(hits, m)
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"skills": hits, "count": len(hits), "query": q})
}

func (s *Server) handleSkill(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/hub/v1/skills/")
	if path == "" {
		s.handleListOrSearch(w, r)
		return
	}
	if strings.HasSuffix(path, "/download") {
		name := strings.TrimSuffix(path, "/download")
		s.handleDownload(w, r, name)
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errMsg("GET only"))
		return
	}
	all := s.storage.List()
	for _, m := range all {
		if m.Name == path {
			writeJSON(w, http.StatusOK, m)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, errMsg("Skill not found"))
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errMsg("GET only"))
		return
	}
	all := s.storage.List()
	for _, m := range all {
		if m.Name != name {
			continue
		}
		data, err := s.storage.Download(name, m.Version)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errMsg(err.Error()))
			return
		}
		s.storage.IncrementDownload(name, m.Version)
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+"-"+m.Version+`.tar.gz"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}
	writeJSON(w, http.StatusNotFound, errMsg("Skill not found"))
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	all := s.storage.List()
	var totalDL int
	for _, m := range all {
		totalDL += m.Downloads
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"skills":         len(all),
		"totalDownloads": totalDL,
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func tagsMatch(tags []string, q string) bool {
	for _, t := range tags {
		if strings.Contains(strings.ToLower(t), q) {
			return true
		}
	}
	return false
}

func errMsg(s string) map[string]string { return map[string]string{"error": s} }

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// _ silences unused imports during partial builds.
var (
	_ = os.Stat
	_ = io.Copy
)
