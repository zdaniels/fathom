package collab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func TestWorkspaceMembershipAndRevisionIsolation(t *testing.T) {
	s := newStore(t)
	a, _ := s.CreateWorkspace("Alpha", "alice")
	b, _ := s.CreateWorkspace("Beta", "bob")
	if err := s.SetMember(a.ID, "bob", "bob", "admin"); !errors.Is(err, ErrForbidden) {
		t.Fatal("nonmember changed roles", err)
	}
	if err := s.SetMember(a.ID, "alice", "alice", ""); err == nil {
		t.Fatal("removed last admin")
	}
	if err := s.SetMember(a.ID, "alice", "bob", "member"); err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(Task{WorkspaceID: a.ID, Title: "Implement"}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Task(b.ID, task.ID); err == nil {
		t.Fatal("cross-workspace task read")
	}
	task.State = "running"
	updated, err := s.Save(task, "alice", "start", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Save(task, "bob", "start", ""); !errors.Is(err, ErrConflict) {
		t.Fatal("stale task start succeeded", err)
	}
	second, _ := s.CreateTask(Task{WorkspaceID: a.ID, Title: "Other"}, "bob")
	second.State = "running"
	if _, err = s.Save(second, "bob", "start", ""); err == nil {
		t.Fatal("concurrent workspace writer accepted")
	}
	if err = s.Recover(); err != nil {
		t.Fatal(err)
	}
	updated, err = s.Task(a.ID, updated.ID)
	if err != nil || updated.State != "blocked" {
		t.Fatal("interrupted run not recovered", err)
	}
	events, _ := s.Activity(a.ID)
	if len(events) < 4 {
		t.Fatal("missing durable activity")
	}
}
func TestConcurrentTaskStartsHaveOneWinner(t *testing.T) {
	s := newStore(t)
	w, _ := s.CreateWorkspace("Team", "u")
	task, _ := s.CreateTask(Task{WorkspaceID: w.ID, Title: "Race"}, "u")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			task := task
			task.State = "running"
			_, err := s.Save(task, "u", "start", "")
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("%d starts won", wins)
	}
}
func TestBoardHTTPAuthorizationAndApproval(t *testing.T) {
	s := newStore(t)
	w, _ := s.CreateWorkspace("Team", "alice")
	_ = s.SetMember(w.ID, "alice", "viewer", "viewer")
	_ = s.SetMember(w.ID, "alice", "member", "member")
	task, _ := s.CreateTask(Task{WorkspaceID: w.ID, Title: "Task"}, "alice")
	service := &Service{Store: s, Runner: &Runner{}, Auth: func(r *http.Request) (string, error) {
		user := r.Header.Get("Authorization")
		if user == "" {
			return "", errors.New("no token")
		}
		return user, nil
	}, CanCreateWorkspace: func(user string) bool { return user == "alice" }}
	request := func(user, method, path string, body any) int {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(method, "/api/v1/board"+path, bytes.NewReader(b))
		req.Header.Set("Authorization", user)
		out := httptest.NewRecorder()
		service.ServeHTTP(out, req)
		return out.Code
	}
	for _, tc := range []struct {
		user, method, path string
		body               any
		code               int
	}{{"", "GET", "", nil, 401}, {"outsider", "GET", "/" + w.ID, nil, 404}, {"viewer", "POST", "/" + w.ID + "/tasks", map[string]string{"title": "No"}, 403}, {"alice", "POST", "/" + w.ID + "/tasks/" + task.ID + "/approve", map[string]int{"revision": 1}, 409}, {"alice", "POST", "/" + w.ID + "/tasks/" + task.ID + "/build", map[string]int{"revision": 1}, 503}, {"member", "POST", "/" + w.ID + "/tasks/" + task.ID + "/build", map[string]int{"revision": 1}, 503}, {"member", "POST", "", map[string]string{"name": "Cannot provision"}, 403}} {
		if got := request(tc.user, tc.method, tc.path, tc.body); got != tc.code {
			t.Fatalf("%s %s got %d want %d", tc.user, tc.path, got, tc.code)
		}
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestLinearConnectionScopeAndErrors(t *testing.T) {
	response := `{"data":{"issue":{"id":"id-123","title":"Build it","description":"Brief","url":"https://linear.app/team/issue/T-1","team":{"id":"team"}}}}`
	c := &Connector{Config: Connection{Provider: "linear", Scope: "team", TokenSecret: "KEY"}, Lookup: func(name string) (string, error) { return "private-key", nil }, Client: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.linear.app/graphql" || r.Header.Get("Authorization") != "private-key" {
			t.Fatal("bad authenticated request")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response))}, nil
	})}}
	task, err := c.Import(context.Background(), "T-1")
	if err != nil || task.Title != "Build it" {
		t.Fatal(task, err)
	}
	c.Config.Scope = "other"
	if _, err = c.Import(context.Background(), "T-1"); err == nil {
		t.Fatal("cross-team import")
	}
	c.Config.Scope = "team"
	response = `{"errors":[{"message":"denied"}],"data":{"issue":null}}`
	if _, err = c.Import(context.Background(), "T-1"); err == nil {
		t.Fatal("GraphQL errors ignored")
	}
}
func TestJiraADFAndSiteValidation(t *testing.T) {
	c := &Connector{Config: Connection{Provider: "jira", Scope: "PROJ", Site: "https://example.atlassian.net", Email: "a@example.com", TokenSecret: "TOKEN"}, Lookup: func(string) (string, error) { return "secret", nil }, Client: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/rest/api/3/issue/PROJ-1" {
			t.Fatal(r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"key":"PROJ-1","fields":{"summary":"Task","project":{"key":"PROJ"},"description":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Brief"}]}]}}}`))}, nil
	})}}
	task, err := c.Import(context.Background(), "PROJ-1")
	if err != nil || task.Description != "Brief" {
		t.Fatal(task, err)
	}
	c.Config.Site = "http://127.0.0.1"
	if _, err = c.Import(context.Background(), "PROJ-1"); err == nil {
		t.Fatal("unsafe endpoint accepted")
	}
}
