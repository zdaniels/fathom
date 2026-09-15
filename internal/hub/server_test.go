package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(Options{
		Host:    "127.0.0.1",
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestServerHealth(t *testing.T) {
	s := newTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/hub/v1/health", nil)
	s.handleHealth(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("health = %d, want 200", w.Code)
	}
}

func TestServerListEmpty(t *testing.T) {
	s := newTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/hub/v1/skills", nil)
	s.handleListOrSearch(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var body map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["count"].(float64) != 0 {
		t.Errorf("empty hub count = %v, want 0", body["count"])
	}
}

func TestServerListAndSearch(t *testing.T) {
	s := newTestServer(t)
	now := time.Now().UTC()
	_ = s.storage.Store("gmail", "1.0.0", []byte("g"),
		Metadata{Name: "gmail", Version: "1.0.0", Description: "send email", Tags: []string{"comms"}, PublishedAt: now})
	_ = s.storage.Store("slack", "1.0.0", []byte("s"),
		Metadata{Name: "slack", Version: "1.0.0", Description: "send messages", Tags: []string{"comms"}, PublishedAt: now.Add(time.Minute)})

	// Bare list returns both.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/hub/v1/skills", nil)
	s.handleListOrSearch(w, r)
	var body map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["count"].(float64) != 2 {
		t.Errorf("list count = %v, want 2", body["count"])
	}

	// Search by name.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/hub/v1/skills?q=gmail", nil)
	s.handleListOrSearch(w2, r2)
	var body2 map[string]interface{}
	_ = json.Unmarshal(w2.Body.Bytes(), &body2)
	if body2["count"].(float64) != 1 {
		t.Errorf("search 'gmail' count = %v, want 1", body2["count"])
	}

	// Search by tag (both share 'comms').
	w3 := httptest.NewRecorder()
	r3 := httptest.NewRequest(http.MethodGet, "/hub/v1/skills?q=comms", nil)
	s.handleListOrSearch(w3, r3)
	var body3 map[string]interface{}
	_ = json.Unmarshal(w3.Body.Bytes(), &body3)
	if body3["count"].(float64) != 2 {
		t.Errorf("search 'comms' count = %v, want 2", body3["count"])
	}
}

func TestServerGetSkillByName(t *testing.T) {
	s := newTestServer(t)
	_ = s.storage.Store("notes", "0.1.0", []byte("n"),
		Metadata{Name: "notes", Version: "0.1.0", Description: "take notes", PublishedAt: time.Now().UTC()})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/hub/v1/skills/notes", nil)
	s.handleSkill(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var meta Metadata
	_ = json.Unmarshal(w.Body.Bytes(), &meta)
	if meta.Name != "notes" {
		t.Errorf("got %+v", meta)
	}
}

func TestServerGetSkillNotFound(t *testing.T) {
	s := newTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/hub/v1/skills/missing", nil)
	s.handleSkill(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestServerDownloadIncrementsCount(t *testing.T) {
	s := newTestServer(t)
	_ = s.storage.Store("notes", "0.1.0", []byte("payload"),
		Metadata{Name: "notes", Version: "0.1.0", PublishedAt: time.Now().UTC()})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/hub/v1/skills/notes/download", nil)
	s.handleSkill(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("download status = %d", w.Code)
	}
	if string(w.Body.Bytes()) != "payload" {
		t.Errorf("body = %q, want 'payload'", w.Body.String())
	}
	meta, _ := s.storage.GetMetadata("notes", "0.1.0")
	if meta.Downloads != 1 {
		t.Errorf("Downloads = %d, want 1", meta.Downloads)
	}
}

func TestServerStats(t *testing.T) {
	s := newTestServer(t)
	_ = s.storage.Store("a", "1", []byte("a"), Metadata{Name: "a", Version: "1", Downloads: 3, PublishedAt: time.Now()})
	_ = s.storage.Store("b", "1", []byte("b"), Metadata{Name: "b", Version: "1", Downloads: 7, PublishedAt: time.Now()})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/hub/v1/stats", nil)
	s.handleStats(w, r)
	var body map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["skills"].(float64) != 2 || body["totalDownloads"].(float64) != 10 {
		t.Errorf("stats = %v", body)
	}
}

func TestServerListMethodNotAllowed(t *testing.T) {
	s := newTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/hub/v1/skills", nil)
	s.handleListOrSearch(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /skills = %d, want 405", w.Code)
	}
}
