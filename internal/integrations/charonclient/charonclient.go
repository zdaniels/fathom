// Package charonclient is Fathom's adapter for talking to an external
// Charon egress proxy (https://github.com/fantazmai/charon).
//
// Fathom has an internal security.EgressProxy that does the same job
// in-process. When cfg.Egress.Proxy is set, Fathom skips standing up
// the internal proxy and points all outbound credential-injected calls
// at Charon instead. The wire protocol Charon expects is small enough
// that this client is ~100 lines — see the Charon repo for the spec.
//
// Bolt-on logic for skills: a skill's ctx.fetch (provided by the runner)
// uses HTTP_PROXY at the OS level. We set FANTAZM_PROXY_URL or pass it
// via the Chasm runtime's EgressProxy field; the skill code itself
// doesn't need to know whether the proxy is the internal one or Charon.
package charonclient

import (
	"fmt"
	"os"
	"strings"
)

// Config captures what Fathom needs to talk to a Charon instance.
type Config struct {
	URL   string // e.g. "http://127.0.0.1:8889"
	Token string // X-Charon-Auth header value
}

// LoadFromFile reads the token from a file path (with ~ expansion).
// Returns ("", nil) if path is empty, mirroring "skip if unset".
func LoadTokenFromFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	expanded := expandTilde(path)
	raw, err := os.ReadFile(expanded)
	if err != nil {
		return "", fmt.Errorf("read charon token file %s: %w", expanded, err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// Validate sanity-checks the config — URL must be set if anything's set
// at all; token must be resolvable from one of the inputs.
func (c *Config) Validate() error {
	if c.URL == "" {
		return fmt.Errorf("charon: URL is required when egress is configured")
	}
	if c.Token == "" {
		return fmt.Errorf("charon: token (or token_file) is required")
	}
	if !strings.HasPrefix(c.URL, "http://") && !strings.HasPrefix(c.URL, "https://") {
		return fmt.Errorf("charon: URL %q must include scheme", c.URL)
	}
	return nil
}

// ProxyURL returns the URL skills see as their HTTP_PROXY. Identical to
// c.URL today — kept as a function so we can do per-skill URL rewriting
// later (e.g. embedding the skill name in the URL path so Charon can
// scope policy per-call).
func (c *Config) ProxyURL() string {
	return c.URL
}

// expandTilde turns a leading "~/" into the user's home dir. Avoids
// importing path/filepath here just to do this; manual expansion keeps
// the deps minimal.
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
