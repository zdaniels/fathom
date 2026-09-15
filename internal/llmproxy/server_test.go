package llmproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestSetup builds a Server fronting a single upstream stub that
// echoes the request body it sees + a fixed usage block. Tests can
// inspect what the stub received to verify the proxy's request
// rewriting (auth header, model field, etc).
func newTestSetup(t *testing.T) (srv *Server, upstreamHits *upstreamRecorder) {
	t.Helper()
	rec := &upstreamRecorder{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.lastPath = r.URL.Path
		rec.lastAuth = r.Header.Get("Authorization")
		rec.lastXAPI = r.Header.Get("x-api-key")
		body, _ := io.ReadAll(r.Body)
		rec.lastBody = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`))
	}))
	t.Cleanup(upstream.Close)

	router, err := NewRouter([]ProviderConfig{{
		Name: "stub", BaseURL: upstream.URL, APIKey: SecretRef{Literal: "test-key"},
	}}, EnvResolver)
	if err != nil {
		t.Fatal(err)
	}
	tenants, err := NewTenantStore([]TenantConfig{{
		ID: "t1", BearerTokens: []SecretRef{{Literal: "bearer-t1"}},
		AllowedModels: []string{"stub/*"},
	}}, EnvResolver)
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(router, tenants, nil), rec
}

type upstreamRecorder struct {
	lastPath, lastAuth, lastXAPI string
	lastBody                     []byte
}

func TestChatCompletionsHappyPath(t *testing.T) {
	srv, up := newTestSetup(t)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"stub/gpt-test","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer bearer-t1")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if up.lastPath != "/chat/completions" {
		t.Errorf("upstream path = %q, want /chat/completions", up.lastPath)
	}
	if up.lastAuth != "Bearer test-key" {
		t.Errorf("upstream auth = %q, want 'Bearer test-key'", up.lastAuth)
	}
	// The "<provider>/" prefix should have been stripped from the model.
	var sent map[string]interface{}
	_ = json.Unmarshal(up.lastBody, &sent)
	if sent["model"] != "gpt-test" {
		t.Errorf("upstream model field = %v, want 'gpt-test'", sent["model"])
	}
}

func TestUnauthorizedWithoutBearer(t *testing.T) {
	srv, _ := newTestSetup(t)
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"stub/x"}`))
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestUnknownProvider(t *testing.T) {
	srv, _ := newTestSetup(t)
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"bogus/x"}`))
	req.Header.Set("Authorization", "Bearer bearer-t1")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (unknown provider)", rr.Code)
	}
}

func TestForbiddenWhenModelNotInAllowlist(t *testing.T) {
	// Tenant only allows "stub/gpt-*"; request a "stub/claude-*" model.
	router, _ := NewRouter([]ProviderConfig{{
		Name: "stub", BaseURL: "http://x", APIKey: SecretRef{Literal: "k"},
	}}, EnvResolver)
	tenants, _ := NewTenantStore([]TenantConfig{{
		ID: "t", BearerTokens: []SecretRef{{Literal: "b"}}, AllowedModels: []string{"stub/gpt-*"},
	}}, EnvResolver)
	srv := NewServer(router, tenants, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"stub/claude-3"}`))
	req.Header.Set("Authorization", "Bearer b")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
	}
}

func TestXApiKeyAuthStyle(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"usage":{"prompt_tokens":0,"completion_tokens":0}}`))
		// We check the header here directly.
		if r.Header.Get("x-api-key") != "K" {
			t.Errorf("upstream x-api-key = %q, want 'K'", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("Authorization should not be set when AuthStyle=x-api-key, got %q", r.Header.Get("Authorization"))
		}
	}))
	t.Cleanup(upstream.Close)
	router, _ := NewRouter([]ProviderConfig{{
		Name: "x", BaseURL: upstream.URL, APIKey: SecretRef{Literal: "K"}, AuthStyle: "x-api-key",
	}}, EnvResolver)
	tenants, _ := NewTenantStore([]TenantConfig{{
		ID: "t", BearerTokens: []SecretRef{{Literal: "b"}}, AllowedModels: []string{"x/*"},
	}}, EnvResolver)
	srv := NewServer(router, tenants, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"x/anything"}`))
	req.Header.Set("Authorization", "Bearer b")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

func TestQuotaEnforcement(t *testing.T) {
	// 2 RPM cap; third request must 429.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(upstream.Close)
	router, _ := NewRouter([]ProviderConfig{{
		Name: "s", BaseURL: upstream.URL, APIKey: SecretRef{Literal: "k"},
	}}, EnvResolver)
	tenants, _ := NewTenantStore([]TenantConfig{{
		ID: "t", BearerTokens: []SecretRef{{Literal: "b"}},
		AllowedModels: []string{"s/*"}, Quotas: Quotas{RequestsPerMinute: 2},
	}}, EnvResolver)
	srv := NewServer(router, tenants, nil)

	doRequest := func() int {
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			bytes.NewBufferString(`{"model":"s/m"}`))
		req.Header.Set("Authorization", "Bearer b")
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr.Code
	}
	if got := doRequest(); got != 200 {
		t.Errorf("1st request = %d, want 200", got)
	}
	if got := doRequest(); got != 200 {
		t.Errorf("2nd request = %d, want 200", got)
	}
	if got := doRequest(); got != http.StatusTooManyRequests {
		t.Errorf("3rd request = %d, want 429", got)
	}
}

func TestModelsEndpointReturnsAllowedGlobs(t *testing.T) {
	srv, _ := newTestSetup(t)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer bearer-t1")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"stub/*"`) {
		t.Errorf("response missing allowed glob: %s", rr.Body.String())
	}
}

func TestUniqueBearerTokensEnforced(t *testing.T) {
	_, err := NewTenantStore([]TenantConfig{
		{ID: "a", BearerTokens: []SecretRef{{Literal: "same"}}, AllowedModels: []string{"x/*"}},
		{ID: "b", BearerTokens: []SecretRef{{Literal: "same"}}, AllowedModels: []string{"x/*"}},
	}, EnvResolver)
	if err == nil {
		t.Error("expected error on duplicate bearer token across tenants")
	}
}

func TestConfigValidate(t *testing.T) {
	c := &Config{}
	if err := c.Validate(); err == nil {
		t.Error("empty config should fail validation")
	}
	c.Providers = []ProviderConfig{{Name: "x"}}
	if err := c.Validate(); err == nil {
		t.Error("provider missing baseUrl should fail")
	}
	c.Providers[0].BaseURL = "https://x"
	c.Tenants = []TenantConfig{{ID: "t"}}
	if err := c.Validate(); err == nil {
		t.Error("tenant with no bearer tokens should fail")
	}
	c.Tenants[0].BearerTokens = []SecretRef{{Literal: "k"}}
	if err := c.Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}
