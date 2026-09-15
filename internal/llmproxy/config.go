// Package llmproxy is the shared HTTP gateway used by the fathom-gateway
// binary. It exposes an OpenAI-compatible /v1/chat/completions surface,
// authenticates incoming requests against a tenant store, and proxies
// the call to one of the configured upstream providers.
//
// v0.1 only proxies to OpenAI-protocol upstreams (openai, gemini,
// deepseek, xai, xiaomi, groq, openrouter, together, fireworks, mistral,
// ollama, lmstudio). Anthropic-native and Bedrock-native upstreams need
// a request-shape translation and are deferred to v0.2.
package llmproxy

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the gateway's YAML schema. Loaded once at startup; nothing
// here mutates at runtime.
type Config struct {
	Listen  string `yaml:"listen"`            // ":8791" etc.
	DataDir string `yaml:"dataDir,omitempty"` // reserved for usage/quotas DB later

	Providers []ProviderConfig `yaml:"providers"`
	Tenants   []TenantConfig   `yaml:"tenants"`
	Auth      AuthConfig       `yaml:"auth"`
	Audit     AuditConfig      `yaml:"audit"`

	// Routing rules apply when a request's model field has no provider
	// prefix (no "<provider>/" segment). v0.1 keeps this empty — callers
	// must use the prefix form.
	Routing []RoutingRule `yaml:"routing,omitempty"`
}

// ProviderConfig is one upstream endpoint. Name is what tenants reference
// in their allowedModels globs (e.g. "anthropic" → "anthropic/claude-*").
// BaseURL must point at the upstream's OpenAI-compatible /v1 root.
type ProviderConfig struct {
	Name    string    `yaml:"name"`
	BaseURL string    `yaml:"baseUrl"`
	APIKey  SecretRef `yaml:"apiKey,omitempty"`
	// AuthStyle controls how the API key is sent upstream. Default
	// "bearer" (the OpenAI standard). "x-api-key" sends as that header
	// instead (Anthropic-style, MiMo also accepts it). "header:NAME"
	// sends as a custom header. "none" skips auth (LM Studio, Ollama).
	AuthStyle string `yaml:"authStyle,omitempty"`
	// Headers are extra static headers added to every request — useful
	// for Anthropic's anthropic-version, OpenRouter's HTTP-Referer
	// for attribution, etc.
	Headers map[string]string `yaml:"headers,omitempty"`
}

// TenantConfig declares one tenant: who can call the gateway, which
// models they're allowed, and what quota they're capped at.
type TenantConfig struct {
	ID            string      `yaml:"id"`
	BearerTokens  []SecretRef `yaml:"bearerTokens"`
	AllowedModels []string    `yaml:"allowedModels"` // globs over "provider/model"
	Quotas        Quotas      `yaml:"quotas,omitempty"`
}

// Quotas constrains tenant usage. Zero means unlimited. Token counting
// reads the upstream's usage field in the response; gateways behind
// upstreams that don't return usage will under-count (treated as 0).
type Quotas struct {
	TokensPerDay      int64 `yaml:"tokensPerDay,omitempty"`
	RequestsPerMinute int   `yaml:"requestsPerMinute,omitempty"`
}

// AuthConfig is the gateway's incoming-auth setup. v0.1 supports bearer
// tokens (per-tenant, configured under TenantConfig.BearerTokens). SSO
// for the admin surface is a v0.2 addition.
type AuthConfig struct {
	Bearer BearerAuth `yaml:"bearer"`
}

type BearerAuth struct {
	Enabled bool `yaml:"enabled"`
}

// AuditConfig picks the audit sink. "stdout" (default) writes JSON
// lines to stderr; "otlp" emits via OTLP logs (deferred — wire-up
// uses Beacon's client in a follow-up).
type AuditConfig struct {
	Sink       string `yaml:"sink,omitempty"`
	MaxEntries int    `yaml:"maxEntries,omitempty"`
}

// RoutingRule is for v0.2 — selects an upstream when no provider prefix
// is present. Carried in the schema now so configs don't have to migrate.
type RoutingRule struct {
	Match     string   `yaml:"match"`
	Upstreams []string `yaml:"upstreams"`
	Strategy  string   `yaml:"strategy,omitempty"`
}

// SecretRef points at where a secret value actually lives. Exactly one
// of Vault / Env / Literal should be set. Literal is discouraged but
// supported so smoke tests don't need a vault running.
type SecretRef struct {
	Vault   string `yaml:"vault,omitempty"`
	Env     string `yaml:"env,omitempty"`
	Literal string `yaml:"literal,omitempty"`
}

// LoadConfig parses a YAML file and runs the minimum validation that
// would otherwise blow up at runtime (duplicate provider name, tenant
// without bearer tokens, etc.). It does NOT resolve secrets — that
// happens at NewServer time so vault availability isn't a config-load
// concern.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate checks structural invariants. Called by LoadConfig but
// exposed for tests that build configs in code.
func (c *Config) Validate() error {
	if c.Listen == "" {
		c.Listen = ":8791"
	}
	if c.Audit.Sink == "" {
		c.Audit.Sink = "stdout"
	}
	if c.Audit.MaxEntries == 0 {
		c.Audit.MaxEntries = 10_000
	}
	if len(c.Providers) == 0 {
		return fmt.Errorf("config: at least one provider required")
	}
	seen := map[string]bool{}
	for i, p := range c.Providers {
		if p.Name == "" {
			return fmt.Errorf("config: providers[%d].name required", i)
		}
		if seen[p.Name] {
			return fmt.Errorf("config: duplicate provider name %q", p.Name)
		}
		seen[p.Name] = true
		if p.BaseURL == "" {
			return fmt.Errorf("config: providers[%d].baseUrl required for %q", i, p.Name)
		}
	}
	tseen := map[string]bool{}
	for i, t := range c.Tenants {
		if t.ID == "" {
			return fmt.Errorf("config: tenants[%d].id required", i)
		}
		if tseen[t.ID] {
			return fmt.Errorf("config: duplicate tenant id %q", t.ID)
		}
		tseen[t.ID] = true
		if len(t.BearerTokens) == 0 {
			return fmt.Errorf("config: tenant %q needs at least one bearerTokens entry", t.ID)
		}
	}
	return nil
}
