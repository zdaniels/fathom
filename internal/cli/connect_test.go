package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveGitHubClientID_Precedence(t *testing.T) {
	t.Setenv("FANTAZM_GITHUB_CLIENT_ID", "from-env")
	if got := resolveGitHubClientID("from-flag"); got != "from-flag" {
		t.Fatalf("flag should win: got %q", got)
	}
	if got := resolveGitHubClientID(""); got != "from-env" {
		t.Fatalf("env should be used when no flag: got %q", got)
	}
}

func TestGitHubDeviceFlow_Mock(t *testing.T) {
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/device/code":
			w.Write([]byte(`{"device_code":"DC","user_code":"WXYZ-1234","verification_uri":"https://example/device","expires_in":60,"interval":0}`))
		case "/token":
			polls++
			if polls == 1 {
				w.Write([]byte(`{"error":"authorization_pending"}`)) // not yet
			} else if polls == 2 {
				w.Write([]byte(`{"error":"slow_down"}`)) // back off
			} else {
				w.Write([]byte(`{"access_token":"gho_test123","token_type":"bearer","scope":"repo"}`))
			}
		case "/user":
			w.Write([]byte(`{"login":"octocat"}`))
		}
	}))
	defer srv.Close()

	// Point the package endpoints at the mock.
	oldD, oldT, oldU := ghDeviceCodeURL, ghTokenURL, ghUserURL
	ghDeviceCodeURL, ghTokenURL, ghUserURL = srv.URL+"/device/code", srv.URL+"/token", srv.URL+"/user"
	defer func() { ghDeviceCodeURL, ghTokenURL, ghUserURL = oldD, oldT, oldU }()

	ctx := context.Background()
	dc, err := ghRequestDeviceCode(ctx, "client-123", "repo")
	if err != nil {
		t.Fatalf("device code request: %v", err)
	}
	if dc.UserCode != "WXYZ-1234" || dc.DeviceCode != "DC" {
		t.Fatalf("unexpected device code: %+v", dc)
	}
	// interval=0 keeps the poll loop tight for the test; slow_down bumps it.
	tok, err := ghPollForToken(ctx, "client-123", dc)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if tok != "gho_test123" {
		t.Fatalf("token = %q, want gho_test123", tok)
	}
	if polls < 3 {
		t.Fatalf("expected the poller to retry through pending+slow_down, got %d polls", polls)
	}
	if who := ghWhoAmI(ctx, tok); who != "octocat" {
		t.Fatalf("whoami = %q, want octocat", who)
	}
}

func TestGitHubPoll_AccessDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"error":"access_denied"}`))
	}))
	defer srv.Close()
	old := ghTokenURL
	ghTokenURL = srv.URL
	defer func() { ghTokenURL = old }()
	_, err := ghPollForToken(context.Background(), "c", &ghDeviceCode{DeviceCode: "x", ExpiresIn: 60, Interval: 0})
	if err == nil || err.Error() != "authorization was denied" {
		t.Fatalf("expected access_denied error, got %v", err)
	}
}
