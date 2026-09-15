// Package identity is the client for the Sigil (formerly agent-identity; ZeroID-based) sidecar.
// In team/enterprise mode, Fathom uses ZeroID to mint per-session +
// per-skill credentials with full delegation chains and real-time
// revocation — implementing the OpenID Foundation's "Identity Management
// for Agentic AI" model.
//
// Connection model: HTTP. Fathom holds a long-lived API key (registered
// once with ZeroID at deploy time) and uses it to issue short-lived
// session/skill tokens via token-exchange.
package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to a ZeroID server (self-hosted via fantazmai/sigil
// or hosted at auth.highflame.ai).
type Client struct {
	baseURL string
	apiKey  string
	hc      *http.Client
}

// New constructs a client. Empty baseURL falls back to the hosted ZeroID.
func New(baseURL, apiKey string) *Client {
	if baseURL == "" {
		baseURL = "https://auth.highflame.ai"
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		hc:      &http.Client{Timeout: 30 * time.Second},
	}
}

// AgentToken is the response from a token-issue call.
type AgentToken struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	WIMSEURI    string `json:"wimse_uri,omitempty"`
}

// MintSessionToken issues a token bound to a Fathom session. The session's
// userID flows into the `owner` claim; ZeroID returns a short-lived bearer
// the gateway can pass to upstream APIs.
func (c *Client) MintSessionToken(ctx context.Context, sessionID, userID string, scopes []string) (*AgentToken, error) {
	form := url.Values{
		"grant_type": {"client_credentials"},
		"scope":      {strings.Join(scopes, " ")},
		"session_id": {sessionID},
		"user_id":    {userID},
	}
	return c.tokenRequest(ctx, "/oauth/token", form)
}

// MintSkillToken does an RFC 8693 token-exchange: it takes a session token
// and delegates a subset of its scope to a specific skill. The resulting
// token's `act` claim records the delegation chain.
func (c *Client) MintSkillToken(ctx context.Context, subjectToken, skillName string, scopes []string) (*AgentToken, error) {
	form := url.Values{
		"grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":        {subjectToken},
		"subject_token_type":   {"urn:ietf:params:oauth:token-type:access_token"},
		"requested_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"scope":                {strings.Join(scopes, " ")},
		"resource":             {"skill:" + skillName},
	}
	return c.tokenRequest(ctx, "/oauth/token", form)
}

// Revoke marks a token as revoked. Downstream tokens in the delegation chain
// are invalidated server-side via CAE.
func (c *Client) Revoke(ctx context.Context, token string) error {
	form := url.Values{"token": {token}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/oauth/revoke", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("identity revoke failed (%d): %s", resp.StatusCode, string(body))
	}
	return nil
}

func (c *Client) tokenRequest(ctx context.Context, path string, form url.Values) (*AgentToken, error) {
	if c.apiKey == "" {
		return nil, errors.New("identity client: no API key configured")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+path, bytes.NewReader([]byte(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("identity token issuance failed (%d): %s", resp.StatusCode, string(body))
	}
	var tok AgentToken
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, err
	}
	return &tok, nil
}
