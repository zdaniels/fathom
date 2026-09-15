package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// oauthFlow runs a localhost OAuth 2.0 authorization-code flow.
//
//  1. Picks a free localhost port and starts an HTTP server.
//  2. Builds the consent URL with redirect_uri pointing at the server.
//  3. Prints the URL for the user to open. (We don't auto-open the browser
//     — let the user decide which browser/profile to use.)
//  4. Server captures the ?code=... callback.
//  5. Exchanges the code for tokens at the provider's token endpoint.
//  6. Returns (refreshToken, accessToken, error).
//
// Generic over any RFC-6749 provider that accepts standard query params.
type oauthFlow struct {
	AuthURL      string // e.g. https://accounts.google.com/o/oauth2/v2/auth
	TokenURL     string // e.g. https://oauth2.googleapis.com/token
	ClientID     string
	ClientSecret string
	Scopes       []string
	AccessType   string // optional — "offline" for Google to get a refresh_token
	Prompt       string // optional — "consent" forces re-consent so we always get a refresh_token
}

type oauthResult struct {
	RefreshToken string
	AccessToken  string
	ExpiresIn    int
	Scope        string
	TokenType    string
}

func (f *oauthFlow) Run(ctx context.Context, w io.Writer) (*oauthResult, error) {
	// Pick a free port; redirect_uri must match the GCP-registered URI.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("oauth: bind localhost: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	stateBytes := make([]byte, 24)
	if _, err := rand.Read(stateBytes); err != nil {
		return nil, fmt.Errorf("oauth: random state: %w", err)
	}
	state := base64.RawURLEncoding.EncodeToString(stateBytes)

	resultCh := make(chan oauthCallback, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(rw http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(rw, "state mismatch — possible CSRF", http.StatusBadRequest)
			resultCh <- oauthCallback{err: fmt.Errorf("state mismatch")}
			return
		}
		if errVal := q.Get("error"); errVal != "" {
			http.Error(rw, "oauth error: "+errVal, http.StatusBadRequest)
			resultCh <- oauthCallback{err: fmt.Errorf("provider returned error: %s", errVal)}
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(rw, "no code in callback", http.StatusBadRequest)
			resultCh <- oauthCallback{err: fmt.Errorf("no code in callback")}
			return
		}
		_, _ = io.WriteString(rw, "<html><body><h2>Fathom: authorization received</h2><p>You can close this tab and return to your terminal.</p></body></html>")
		resultCh <- oauthCallback{code: code}
	})
	server := &http.Server{Handler: mux, ReadTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	defer func() {
		_, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = server.Close()
		cancel()
	}()

	// Build the consent URL.
	params := url.Values{
		"client_id":     {f.ClientID},
		"response_type": {"code"},
		"redirect_uri":  {redirectURI},
		"scope":         {strings.Join(f.Scopes, " ")},
		"state":         {state},
	}
	if f.AccessType != "" {
		params.Set("access_type", f.AccessType)
	}
	if f.Prompt != "" {
		params.Set("prompt", f.Prompt)
	}
	consentURL := f.AuthURL + "?" + params.Encode()

	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Open this URL in your browser to authorize Fathom:")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  "+consentURL)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Waiting for the callback…")
	fmt.Fprintln(w)

	// Wait for the callback or ctx cancellation.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case cb := <-resultCh:
		if cb.err != nil {
			return nil, cb.err
		}
		return f.exchangeCode(ctx, cb.code, redirectURI)
	case <-time.After(10 * time.Minute):
		return nil, fmt.Errorf("oauth: timed out waiting for user authorization")
	}
}

type oauthCallback struct {
	code string
	err  error
}

// exchangeCode swaps the authorization code for access + refresh tokens.
func (f *oauthFlow) exchangeCode(ctx context.Context, code, redirectURI string) (*oauthResult, error) {
	form := url.Values{
		"code":          {code},
		"client_id":     {f.ClientID},
		"client_secret": {f.ClientSecret},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(cctx, http.MethodPost, f.TokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth: token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth: token exchange returned %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		RefreshToken string `json:"refresh_token"`
		AccessToken  string `json:"access_token"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
		TokenType    string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("oauth: parse token response: %w", err)
	}
	if out.RefreshToken == "" {
		return nil, fmt.Errorf("oauth: provider didn't return a refresh_token — try revoking the existing grant and re-running so the consent screen appears again")
	}
	return &oauthResult{
		RefreshToken: out.RefreshToken,
		AccessToken:  out.AccessToken,
		ExpiresIn:    out.ExpiresIn,
		Scope:        out.Scope,
		TokenType:    out.TokenType,
	}, nil
}
