// Package chasmclient is Fathom's adapter for talking to an external
// Chasm sandbox server (https://github.com/fantazmai/chasm).
//
// Drop-in alternative for the in-process Node-subprocess sandbox
// (internal/skills/sandbox.go). Each skill invocation becomes a single
// HTTP POST to Chasm /run; Chasm spins up a transient hardened
// container, runs the function, returns the JSON result.
//
// Activated by setting `skills.runtime: chasm` in fathom.config.yaml.
package chasmclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Config captures what Fathom needs to talk to a Chasm instance.
type Config struct {
	URL          string // e.g. "http://127.0.0.1:8890"
	Token        string // Authorization: Bearer <Token>
	DefaultImage string // image to use when a skill doesn't specify one
}

// Validate sanity-checks the config.
func (c *Config) Validate() error {
	if c.URL == "" {
		return fmt.Errorf("chasm: URL is required when skills.runtime=chasm")
	}
	if c.Token == "" {
		return fmt.Errorf("chasm: token (or token_file) is required")
	}
	if c.DefaultImage == "" {
		return fmt.Errorf("chasm: default_image is required (e.g. ghcr.io/fantazmai/chasm-node22:latest)")
	}
	if !strings.HasPrefix(c.URL, "http://") && !strings.HasPrefix(c.URL, "https://") {
		return fmt.Errorf("chasm: URL %q must include scheme", c.URL)
	}
	return nil
}

// LoadTokenFromFile reads a token from disk; mirrors charonclient's
// helper so callers don't have to import two different ones.
func LoadTokenFromFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	expanded := expandTilde(path)
	raw, err := os.ReadFile(expanded)
	if err != nil {
		return "", fmt.Errorf("read chasm token file %s: %w", expanded, err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// RunRequest mirrors Chasm's spec.RunRequest. Defined here too so we
// don't need to import the chasm module (which would make Fathom depend
// on a private repo at build time).
type RunRequest struct {
	Image       string            `json:"image"`
	CodeDir     string            `json:"code_dir"`
	Entrypoint  string            `json:"entrypoint"`
	Function    string            `json:"function"`
	Input       any               `json:"input"`
	Secrets     map[string]string `json:"secrets,omitempty"`
	EgressProxy string            `json:"egress_proxy,omitempty"`
	EgressToken string            `json:"egress_token,omitempty"`
	TimeoutMs   int               `json:"timeout_ms,omitempty"`
	MemoryMB    int               `json:"memory_mb,omitempty"`
	CPUs        float64           `json:"cpus,omitempty"`
	Network     string            `json:"network,omitempty"`
}

// RunResponse mirrors Chasm's spec.RunResponse.
type RunResponse struct {
	OK       bool   `json:"ok"`
	Output   any    `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
	ExitCode int    `json:"exit_code"`
	Duration int64  `json:"duration_ms"`
	Stderr   string `json:"stderr,omitempty"`
}

// Client posts RunRequests to a Chasm server.
type Client struct {
	cfg  Config
	http *http.Client
}

// New constructs a Client. The HTTP timeout is generous — Chasm itself
// enforces per-run timeouts via the request's timeout_ms; this just
// caps how long we'll wait for the daemon to respond at all.
func New(cfg Config) *Client {
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: 10 * time.Minute},
	}
}

// Run sends a request and returns the response.
func (c *Client) Run(ctx context.Context, req *RunRequest) (*RunResponse, error) {
	if req.Image == "" {
		req.Image = c.cfg.DefaultImage
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL+"/run", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+c.cfg.Token)

	resp, err := c.http.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("chasm request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Read up to 4KB of the body for context, then surface.
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("chasm returned %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out RunResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode chasm response: %w", err)
	}
	return &out, nil
}

// Healthz pings /healthz; useful at Fathom startup to fail-fast if the
// configured Chasm URL is unreachable.
func (c *Client) Healthz(ctx context.Context) error {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.URL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(hreq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("chasm /healthz returned %s", resp.Status)
	}
	return nil
}

func expandTilde(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return p
	}
	return home + p[1:]
}
