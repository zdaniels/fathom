package enterprise

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/zdaniels/fathom/internal/security"
	"golang.org/x/oauth2"
)

type OIDCConfig struct {
	Issuer, ClientID, ClientSecret, RedirectURI string
	Scopes                                      []string
}
type SSOUser struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
}
type loginAttempt struct {
	nonce, verifier, browser string
	expires                  time.Time
}
type stepUpGrant struct {
	user    string
	expires time.Time
}

// SSOManager owns browser-bound, expiring, single-use login and step-up state.
// Identity is always derived from a verified ID token, never email or groups.
type SSOManager struct {
	cfg      *OIDCConfig
	client   *http.Client
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	mu       sync.Mutex
	pending  map[string]loginAttempt
	grants   map[string]stepUpGrant
}

func NewSSOManager() *SSOManager {
	return &SSOManager{client: &http.Client{Timeout: 15 * time.Second}, pending: map[string]loginAttempt{}, grants: map[string]stepUpGrant{}}
}
func validOIDCURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	return u.Scheme == "https" || (u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1"))
}
func (s *SSOManager) Configure(cfg OIDCConfig) error {
	if !validOIDCURL(cfg.Issuer) || !validOIDCURL(cfg.RedirectURI) || cfg.ClientID == "" {
		return errors.New("SSO requires a client ID and explicit HTTPS issuer and redirect URI (loopback HTTP allowed)")
	}
	u, _ := url.Parse(cfg.RedirectURI)
	if u.Path != "/api/v1/sso/callback" || u.RawQuery != "" {
		return errors.New("SSO redirect URI must end in /api/v1/sso/callback without a query")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ctx = oidc.ClientContext(ctx, s.client)
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return err
	}
	var metadata struct {
		JWKSURL string `json:"jwks_uri"`
	}
	if err := provider.Claims(&metadata); err != nil || !validOIDCURL(metadata.JWKSURL) {
		return errors.New("SSO discovery returned an insecure signing-key endpoint")
	}
	endpoint := provider.Endpoint()
	if !validOIDCURL(endpoint.AuthURL) || !validOIDCURL(endpoint.TokenURL) {
		return errors.New("SSO discovery returned insecure endpoints")
	}
	scopes := append([]string{oidc.ScopeOpenID, "profile", "email"}, cfg.Scopes...)
	s.oauth = oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: cfg.RedirectURI, Endpoint: endpoint, Scopes: scopes}
	s.verifier = provider.Verifier(&oidc.Config{ClientID: cfg.ClientID})
	s.cfg = &cfg
	return nil
}
func (s *SSOManager) IsConfigured() bool { return s.cfg != nil && s.verifier != nil }

// Handle provides login and callback routes. issue creates a bounded gateway
// session; isAdmin consults current persisted roles before granting step-up.
func (s *SSOManager) Handle(w http.ResponseWriter, r *http.Request, issue func(string) (string, error), isAdmin func(string) bool) bool {
	if r.URL.Path != "/api/v1/sso/login" && r.URL.Path != "/api/v1/sso/callback" {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Method != http.MethodGet {
		respJSON(w, 405, map[string]string{"error": "GET only"})
		return true
	}
	if !s.IsConfigured() {
		respJSON(w, 503, map[string]string{"error": "SSO unavailable"})
		return true
	}
	secure := strings.HasPrefix(s.cfg.RedirectURI, "https:")
	if r.URL.Path == "/api/v1/sso/login" {
		state := oauth2.GenerateVerifier()
		nonce := oauth2.GenerateVerifier()
		browser := oauth2.GenerateVerifier()
		verifier := oauth2.GenerateVerifier()
		s.mu.Lock()
		now := time.Now()
		for k, v := range s.pending {
			if now.After(v.expires) {
				delete(s.pending, k)
			}
		}
		if len(s.pending) >= 1024 {
			s.mu.Unlock()
			respJSON(w, 429, map[string]string{"error": "Too many pending logins"})
			return true
		}
		s.pending[state] = loginAttempt{nonce: nonce, verifier: verifier, browser: browser, expires: now.Add(5 * time.Minute)}
		s.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "fathom_oidc", Value: browser, Path: "/api/v1/sso", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: 300})
		dest := s.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("max_age", "0"), oauth2.SetAuthURLParam("prompt", "login"))
		http.Redirect(w, r, dest, http.StatusFound)
		return true
	}
	state := r.URL.Query().Get("state")
	cookie, err := r.Cookie("fathom_oidc")
	s.mu.Lock()
	attempt, ok := s.pending[state]
	if ok && err == nil && subtle.ConstantTimeCompare([]byte(attempt.browser), []byte(cookie.Value)) == 1 {
		delete(s.pending, state)
	} else {
		ok = false
	}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "fathom_oidc", Value: "", Path: "/api/v1/sso", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	if !ok || time.Now().After(attempt.expires) || r.URL.Query().Get("code") == "" {
		respJSON(w, 400, map[string]string{"error": "Invalid or expired login state"})
		return true
	}
	ctx := oidc.ClientContext(r.Context(), s.client)
	tok, err := s.oauth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(attempt.verifier))
	if err != nil {
		respJSON(w, 401, map[string]string{"error": "OIDC code exchange failed"})
		return true
	}
	raw, _ := tok.Extra("id_token").(string)
	id, err := s.verifier.Verify(ctx, raw)
	if err != nil || id.Subject == "" || subtle.ConstantTimeCompare([]byte(id.Nonce), []byte(attempt.nonce)) != 1 {
		respJSON(w, 401, map[string]string{"error": "Invalid ID token"})
		return true
	}
	var claims struct {
		Email    string `json:"email"`
		Name     string `json:"name"`
		AuthTime int64  `json:"auth_time"`
	}
	if id.Claims(&claims) != nil {
		respJSON(w, 401, map[string]string{"error": "Invalid ID claims"})
		return true
	}
	age := time.Since(time.Unix(claims.AuthTime, 0))
	if claims.AuthTime <= 0 || age < -30*time.Second || age > 5*time.Minute {
		respJSON(w, 401, map[string]string{"error": "Fresh authentication required"})
		return true
	}
	// Namespace subjects by issuer, with no ambiguous delimiter concatenation.
	user := "oidc:" + security.SHA256Hex(s.cfg.Issuer+"\x00"+id.Subject)
	token, err := issue(user)
	if err != nil {
		respJSON(w, 500, map[string]string{"error": "Could not create session"})
		return true
	}
	out := map[string]interface{}{"token": token, "expiresIn": 3600, "user": SSOUser{ID: user, Email: claims.Email, Name: claims.Name, Provider: s.cfg.Issuer}}
	// max_age requires auth_time. Missing/stale auth_time never grants step-up.
	now := time.Now()
	age = now.Sub(time.Unix(claims.AuthTime, 0))
	if isAdmin(user) && claims.AuthTime > 0 && age >= -30*time.Second && age <= 5*time.Minute {
		grant := oauth2.GenerateVerifier()
		s.mu.Lock()
		for k, v := range s.grants {
			if now.After(v.expires) {
				delete(s.grants, k)
			}
		}
		s.grants[security.SHA256Hex(grant)] = stepUpGrant{user: user, expires: now.Add(5 * time.Minute)}
		s.mu.Unlock()
		out["stepUpToken"] = grant
	}
	respJSON(w, 200, out)
	return true
}
func (s *SSOManager) ConsumeStepUp(token string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := security.SHA256Hex(token)
	grant, ok := s.grants[key]
	delete(s.grants, key)
	return grant.user, ok && time.Now().Before(grant.expires)
}
