package enterprise

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBrowserLoginEscapesClaimsAndKeepsCredentialsOutOfURL(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/v1/sso/callback", nil)
	r.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	if !finishBrowserLogin(w, r, map[string]interface{}{"token": "secret", "user": SSOUser{ID: "u", Name: "</script><script>alert(1)</script>"}}) {
		t.Fatal("browser flow not selected")
	}
	if strings.Contains(w.Body.String(), "<script>alert") || w.Header().Get("Location") != "" {
		t.Fatal("unsafe browser credential response")
	}
	if w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "nonce-") {
		t.Fatal("missing browser protections")
	}
}
