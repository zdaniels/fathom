package collab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zdaniels/fathom/internal/security"
)

const mockTeam = "11111111-1111-4111-8111-111111111111"

func TestManageConnectionsPersistenceSecretsAndScope(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	vaultPath := filepath.Join(dir, "vault")
	key := bytes.Repeat([]byte{7}, 32)
	vault, err := security.OpenVault(security.VaultOpenOptions{Path: vaultPath, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	ws, _ := store.CreateWorkspace("Team", "admin")
	other, _ := store.CreateWorkspace("Other", "admin")
	for _, m := range []Member{{"member", "member"}, {"viewer", "viewer"}, {"workspace-admin", "admin"}} {
		if err = store.SetMember(ws.ID, "admin", m.UserID, m.Role); err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{Store: store, Runner: &Runner{}, Auth: func(r *http.Request) (string, error) { return r.Header.Get("User"), nil }, CanManageConnections: func(_ *http.Request, u string) bool { return u == "admin" }, ConnectionGuard: func(w http.ResponseWriter, r *http.Request, u string) bool {
		if u != "admin" {
			fail(w, 403, ErrForbidden)
			return false
		}
		return true
	}, Lookup: func(name string) (string, error) { return vault.Get(name, "") }, SaveSecret: func(name, value string) error { return vault.Set(name, value, []string{"fathom:board-connector"}) }, DeleteSecret: func(name string) { vault.Delete(name) }}
	requests := 0
	service.ConnectionClient = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Host != "api.linear.app" || r.Header.Get("Authorization") != "mock-secret" {
			t.Error("wrong scope or credential")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{"team":{"id":"` + mockTeam + `"}}}`))}, nil
	})}
	call := func(user, workspace, action string, body any) *httptest.ResponseRecorder {
		method := "GET"
		var data []byte
		if body != nil {
			method = "POST"
			data, _ = json.Marshal(body)
		}
		r := httptest.NewRequest(method, "/api/v1/board/"+workspace+"/connections"+action, bytes.NewReader(data))
		r.Header.Set("User", user)
		w := httptest.NewRecorder()
		service.ServeHTTP(w, r)
		return w
	}
	input := map[string]any{"provider": "linear", "scope": mockTeam, "token": "mock-secret", "revision": 0}
	for _, u := range []string{"member", "viewer", "workspace-admin", "outsider"} {
		w := call(u, ws.ID, "/save", input)
		if w.Code != 403 && w.Code != 404 {
			t.Fatalf("%s configured a credential: %s", u, w.Body.String())
		}
	}
	if w := call("admin", ws.ID, "/check", input); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if records, _ := store.connectionRecords(ws.ID); len(records) != 0 {
		t.Fatal("check persisted configuration")
	}
	if w := call("admin", ws.ID, "/save", input); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if requests != 1 {
		t.Fatal("save unexpectedly contacted external provider")
	}
	view := call("admin", ws.ID, "", nil)
	if strings.Contains(view.Body.String(), "mock-secret") || strings.Contains(view.Body.String(), "fathom-board-") {
		t.Fatal("API exposed credential")
	}
	records, _ := store.connectionRecords(ws.ID)
	ref := records[0].Config.TokenSecret
	if _, err = vault.Get(ref, "unrelated-skill"); err == nil {
		t.Fatal("credential not scoped")
	}
	events, _ := store.Activity(ws.ID)
	b, _ := json.Marshal(events)
	if bytes.Contains(b, []byte("mock-secret")) {
		t.Fatal("credential in activity")
	}
	for _, file := range []string{vaultPath, filepath.Join(dir, "board.db"), filepath.Join(dir, "board.db-wal")} {
		b, _ := os.ReadFile(file)
		if bytes.Contains(b, []byte("mock-secret")) {
			t.Fatal("plaintext credential on disk")
		}
	}
	// Failed/stale writes must leave the prior credential intact.
	if w := call("admin", ws.ID, "/save", input); w.Code != 409 {
		t.Fatal("stale write accepted")
	}
	service.SaveSecret = func(string, string) error { return errors.New("vault unavailable") }
	input["revision"] = 1
	input["token"] = "replacement"
	if w := call("admin", ws.ID, "/save", input); w.Code != 503 {
		t.Fatal("failed vault write accepted")
	}
	if value, _ := vault.Get(ref, ""); value != "mock-secret" {
		t.Fatal("lost previous credential")
	}
	if w := call("admin", ws.ID, "/disable", map[string]any{"provider": "linear", "revision": 1}); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if c, err := service.connector(ws.ID, "linear"); err != nil || c != nil {
		t.Fatal("disabled connector still usable")
	}
	if c, err := service.connector(other.ID, "linear"); err != nil || c != nil {
		t.Fatal("cross-workspace connector exposed")
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(filepath.Join(dir, "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	service.Store = store
	vault, err = security.OpenVault(security.VaultOpenOptions{Path: vaultPath, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	records, _ = store.connectionRecords(ws.ID)
	if len(records) != 1 || records[0].Enabled || records[0].Revision != 2 {
		t.Fatal("disabled state not durable")
	}
	if value, _ := vault.Get(ref, ""); value != "mock-secret" {
		t.Fatal("vault did not survive reopen")
	}
	// Empty token reuses the saved credential and reenables immediately.
	input["token"] = ""
	input["revision"] = 2
	if w := call("admin", ws.ID, "/save", input); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if c, err := service.connector(ws.ID, "linear"); err != nil || c == nil || c.Config.TokenSecret != ref {
		t.Fatal("reenable failed")
	}
}
func TestJiraChecksAndCredentialDestination(t *testing.T) {
	store := newStore(t)
	ws, _ := store.CreateWorkspace("Jira", "admin")
	config := Connection{WorkspaceID: ws.ID, Provider: "jira", Scope: "TEAM", Site: "https://demo.atlassian.net", Email: "demo@example.com", TokenSecret: "EXISTING"}
	service := &Service{Store: store, Connections: []Connection{config}, Auth: func(*http.Request) (string, error) { return "admin", nil }, ConnectionGuard: func(http.ResponseWriter, *http.Request, string) bool { return true }, Lookup: func(string) (string, error) { return "mock-jira-token", nil }}
	calls := 0
	service.ConnectionClient = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		u, p, ok := r.BasicAuth()
		if !ok || u != config.Email || p != "mock-jira-token" || r.URL.String() != "https://demo.atlassian.net/rest/api/3/project/TEAM" {
			t.Error("incorrect Jira verification request")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"key":"TEAM"}`))}, nil
	})}
	request := func(site, action string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"provider": "jira", "site": site, "email": config.Email, "scope": "TEAM", "revision": 0})
		r := httptest.NewRequest("POST", "/api/v1/board/"+ws.ID+"/connections/"+action, bytes.NewReader(body))
		w := httptest.NewRecorder()
		service.ServeHTTP(w, r)
		return w
	}
	if w := request(config.Site, "check"); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, site := range []string{"https://other.atlassian.net", "http://127.0.0.1", "https://demo.atlassian.net.evil.test", "https://user@demo.atlassian.net", "https://demo.atlassian.net/path"} {
		if w := request(site, "check"); w.Code != 400 {
			t.Fatalf("unsafe destination %s: %s", site, w.Body.String())
		}
	}
	if calls != 1 {
		t.Fatal("sent credential to changed destination")
	}
	if w := request(config.Site, "disable"); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if c, err := service.connector(ws.ID, "jira"); c != nil || err != nil {
		t.Fatal("disabled YAML connection fell through")
	}
}

func TestConnectionCheckRejectsProviderFailuresAndRedirects(t *testing.T) {
	for _, response := range []string{`{"errors":[{"message":"mock-secret"}]}`, `{"data":{"team":{"id":"other-team"}}}`, `not-json mock-secret`} {
		connector := &Connector{Config: Connection{Provider: "linear", Scope: mockTeam}, Lookup: func(string) (string, error) { return "mock-secret", nil }, Client: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response))}, nil
		})}}
		err := connector.Check(context.Background())
		if err == nil || strings.Contains(err.Error(), "mock-secret") {
			t.Fatal("provider failure leaked or passed", err)
		}
	}
	calls := 0
	connector := &Connector{Config: Connection{Provider: "linear", Scope: mockTeam}, Lookup: func(string) (string, error) { return "mock-secret", nil }, Client: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://evil.example"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}}
	if err := connector.Check(context.Background()); err == nil || calls != 1 {
		t.Fatal("redirect followed", calls, err)
	}
}
