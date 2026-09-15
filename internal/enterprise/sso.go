package enterprise

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zdaniels/fathom/internal/security"
)

// OIDCConfig holds the issuer + client credentials for a single OIDC provider.
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURI  string
	Scopes       []string
}

// SSOUser is the normalized shape we expose to the rest of the app after a
// successful OIDC exchange.
type SSOUser struct {
	ID        string                 `json:"id"`
	Email     string                 `json:"email"`
	Name      string                 `json:"name"`
	Groups    []string               `json:"groups"`
	Provider  string                 `json:"provider"`
	RawClaims map[string]interface{} `json:"rawClaims"`
}

// SSOManager wraps the OIDC client. mountEnterprise constructs and configures
// one when mode=enterprise and the config has an sso block.
type SSOManager struct {
	cfg    *OIDCConfig
	client *http.Client
}

// NewSSOManager returns an empty manager — call Configure before use.
func NewSSOManager() *SSOManager {
	return &SSOManager{client: &http.Client{Timeout: 30 * time.Second}}
}

// Configure sets the OIDC client. Idempotent.
func (s *SSOManager) Configure(cfg OIDCConfig) { s.cfg = &cfg }

// IsConfigured reports whether Configure has been called.
func (s *SSOManager) IsConfigured() bool { return s.cfg != nil }

// AuthorizationURL builds the redirect URL for the OIDC dance.
func (s *SSOManager) AuthorizationURL(state string) (string, error) {
	if s.cfg == nil {
		return "", errors.New("SSO not configured")
	}
	scopes := s.cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "email", "profile"}
	}
	if state == "" {
		state = security.GenerateID()
	}
	params := url.Values{
		"client_id":     {s.cfg.ClientID},
		"redirect_uri":  {s.cfg.RedirectURI},
		"response_type": {"code"},
		"scope":         {strings.Join(scopes, " ")},
		"state":         {state},
	}
	return s.cfg.Issuer + "/authorize?" + params.Encode(), nil
}

// ExchangeCode swaps an authorization code for a userinfo profile.
func (s *SSOManager) ExchangeCode(code string) (*SSOUser, error) {
	if s.cfg == nil {
		return nil, errors.New("SSO not configured")
	}

	tokForm := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {s.cfg.ClientID},
		"client_secret": {s.cfg.ClientSecret},
		"redirect_uri":  {s.cfg.RedirectURI},
	}
	resp, err := s.client.Post(
		s.cfg.Issuer+"/token",
		"application/x-www-form-urlencoded",
		strings.NewReader(tokForm.Encode()),
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return nil, errors.New("token exchange failed: " + string(body))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return nil, err
	}

	uiReq, _ := http.NewRequest(http.MethodGet, s.cfg.Issuer+"/userinfo", nil)
	uiReq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	uiResp, err := s.client.Do(uiReq)
	if err != nil {
		return nil, err
	}
	defer uiResp.Body.Close()
	if uiResp.StatusCode != http.StatusOK {
		return nil, errors.New("userinfo fetch failed")
	}
	var claims map[string]interface{}
	if err := json.NewDecoder(uiResp.Body).Decode(&claims); err != nil {
		return nil, err
	}
	return userFromClaims(s.cfg.Issuer, claims), nil
}

func userFromClaims(issuer string, claims map[string]interface{}) *SSOUser {
	u := &SSOUser{Provider: issuer, RawClaims: claims}
	if v, ok := claims["sub"].(string); ok {
		u.ID = v
	} else if v, ok := claims["id"].(string); ok {
		u.ID = v
	} else {
		u.ID = security.GenerateID()
	}
	if v, ok := claims["email"].(string); ok {
		u.Email = v
	}
	if v, ok := claims["name"].(string); ok {
		u.Name = v
	}
	if g, ok := claims["groups"].([]interface{}); ok {
		for _, x := range g {
			if s, ok := x.(string); ok {
				u.Groups = append(u.Groups, s)
			}
		}
	}
	return u
}
