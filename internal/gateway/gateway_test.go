package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zdaniels/fathom/pkg/types"
)

func newTestGateway(t *testing.T) (*Gateway, string) {
	t.Helper()
	cfg := types.Config{
		Host: "127.0.0.1",
		Port: 0,
		Auth: types.AuthConfig{SessionTimeout: time.Hour},
	}
	g := New(cfg)
	tok, err := g.Auth.SetupInitialToken()
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	return g, tok
}

func TestGatewayHealthReturnsOK(t *testing.T) {
	g, _ := newTestGateway(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	g.handleHealth(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200", w.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body unmarshal: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %v, want 'ok'", body["status"])
	}
}

func TestGatewayHealthRejectsPOST(t *testing.T) {
	g, _ := newTestGateway(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/health", nil)
	g.handleHealth(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST to /health = %d, want 405", w.Code)
	}
}

func TestGatewayMessageRequiresAuth(t *testing.T) {
	g, _ := newTestGateway(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/message",
		strings.NewReader(`{"text":"hi"}`))
	g.handleMessage(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no-auth POST = %d, want 401", w.Code)
	}
}

func TestGatewayMessageBadJSON(t *testing.T) {
	g, tok := newTestGateway(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/message",
		strings.NewReader(`not json`))
	r.Header.Set("Authorization", "Bearer "+tok)
	g.handleMessage(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad-json = %d, want 400", w.Code)
	}
}

func TestGatewayMessageEmptyText(t *testing.T) {
	g, tok := newTestGateway(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/message",
		strings.NewReader(`{"text":""}`))
	r.Header.Set("Authorization", "Bearer "+tok)
	g.handleMessage(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty text = %d, want 400", w.Code)
	}
}

func TestGatewayMessageNoHandlerYields503(t *testing.T) {
	g, tok := newTestGateway(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/message",
		strings.NewReader(`{"text":"hi"}`))
	r.Header.Set("Authorization", "Bearer "+tok)
	g.handleMessage(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("no handler = %d, want 503", w.Code)
	}
}

func TestGatewayMessageDispatchesToHandler(t *testing.T) {
	g, tok := newTestGateway(t)
	var seenText string
	g.SetMessageHandler(func(ctx context.Context, msg types.ChannelMessage, _ types.Session) (string, error) {
		seenText = msg.Text
		return "ack: " + msg.Text, nil
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/message",
		strings.NewReader(`{"text":"ping"}`))
	r.Header.Set("Authorization", "Bearer "+tok)
	g.handleMessage(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if seenText != "ping" {
		t.Errorf("handler text = %q, want 'ping'", seenText)
	}
	var body map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["response"] != "ack: ping" {
		t.Errorf("response = %v, want 'ack: ping'", body["response"])
	}
}

func TestGatewayMessagePropagatesHandlerError(t *testing.T) {
	g, tok := newTestGateway(t)
	g.SetMessageHandler(func(ctx context.Context, msg types.ChannelMessage, _ types.Session) (string, error) {
		return "", errors.New("LLM exploded")
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/message",
		strings.NewReader(`{"text":"x"}`))
	r.Header.Set("Authorization", "Bearer "+tok)
	g.handleMessage(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("handler-err status = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "LLM exploded") {
		t.Errorf("body should include error, got %s", w.Body.String())
	}
}

func TestGatewaySessionRequiresGET(t *testing.T) {
	g, tok := newTestGateway(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/session", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	g.handleGetSession(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /session = %d, want 405", w.Code)
	}
}

func TestGatewayFallbackToExtensionHandler(t *testing.T) {
	g, _ := newTestGateway(t)
	called := false
	g.SetExtensionHandler(func(w http.ResponseWriter, r *http.Request) bool {
		called = true
		w.WriteHeader(http.StatusTeapot)
		return true
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/admin/whatever", nil)
	g.handleFallback(w, r)
	if !called {
		t.Error("extension hook not invoked")
	}
	if w.Code != http.StatusTeapot {
		t.Errorf("status = %d, want 418 from hook", w.Code)
	}
}

func TestGatewayFallbackToNotFoundWhenHookSkips(t *testing.T) {
	g, _ := newTestGateway(t)
	g.SetExtensionHandler(func(http.ResponseWriter, *http.Request) bool { return false })
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/no/such/route", nil)
	g.handleFallback(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestGatewayServesIndexAtRoot(t *testing.T) {
	// Root / legacy aliases must all serve the PWA shell — regression
	// guard for the mobile-UI rewrite that moved chat.html → index.html.
	g, _ := newTestGateway(t)
	for _, path := range []string{"/", "/chat", "/chat.html"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		g.handleFallback(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET %s content-type = %q, want text/html", path, ct)
		}
		if !strings.Contains(w.Body.String(), "manifest.webmanifest") {
			t.Errorf("GET %s did not contain PWA manifest link — wrong file served?", path)
		}
	}
}

func TestGatewayServesPWAAssets(t *testing.T) {
	g, _ := newTestGateway(t)
	cases := []struct {
		path string
		ct   string
	}{
		{"/chat.css", "text/css"},
		{"/chat.js", "application/javascript"},
		{"/manifest.webmanifest", "application/manifest+json"},
		{"/sw.js", "application/javascript"},
		{"/icons/icon-192.png", "image/png"},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, c.path, nil)
		g.handleFallback(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", c.path, w.Code)
			continue
		}
		if !strings.HasPrefix(w.Header().Get("Content-Type"), c.ct) {
			t.Errorf("GET %s content-type = %q, want prefix %q", c.path, w.Header().Get("Content-Type"), c.ct)
		}
		if w.Body.Len() == 0 {
			t.Errorf("GET %s returned empty body", c.path)
		}
	}
}

func TestGatewayServiceWorkerIsNoStore(t *testing.T) {
	// Critical: if sw.js gets aggressively cached, users see stale UI for
	// days. The handler must mark it no-store so the browser always asks.
	g, _ := newTestGateway(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/sw.js", nil)
	g.handleFallback(w, r)
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("sw.js cache-control = %q, want no-store", cc)
	}
}

func TestGatewayMessageBodySizeLimit(t *testing.T) {
	// Body capped at 1MiB. Sending a slightly oversized text payload should
	// still succeed because the limit reader caps the read, but the JSON will
	// be truncated → unmarshal fails → 400.
	g, tok := newTestGateway(t)
	huge := strings.Repeat("a", 1024*1024+1024)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/message",
		strings.NewReader(`{"text":"`+huge+`"}`))
	r.Header.Set("Authorization", "Bearer "+tok)
	g.handleMessage(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("oversized body = %d, want 400 (truncated json fails unmarshal)", w.Code)
	}
}
