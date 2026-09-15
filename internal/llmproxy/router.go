package llmproxy

import (
	"fmt"
	"net/url"
	"strings"
)

// Provider is a resolved upstream — the ProviderConfig with its secret
// fields populated. Built by NewRouter so the rest of the request path
// doesn't have to think about secret resolution per request.
type Provider struct {
	Name      string
	BaseURL   string
	APIKey    string
	AuthStyle string
	Headers   map[string]string
}

// Router maps incoming requests to a specific upstream Provider. v0.1
// requires the request's `model` field to be in "<provider>/<model>"
// form — the prefix selects the provider, the rest is forwarded as-is.
// RoutingRule-based selection (slug → upstream) lands in v0.2.
type Router struct {
	byName map[string]*Provider
	order  []string // stable list for ListProviders()
}

// NewRouter resolves every provider's secrets up front and stashes the
// flat Provider records. Returns an error if any provider's secret
// resolution fails — partial bring-up is too easy to miss in prod.
func NewRouter(cfgs []ProviderConfig, resolve SecretResolver) (*Router, error) {
	r := &Router{byName: map[string]*Provider{}}
	for _, c := range cfgs {
		key := ""
		if c.AuthStyle != "none" && (c.APIKey.Vault != "" || c.APIKey.Env != "" || c.APIKey.Literal != "") {
			v, err := resolve(c.APIKey)
			if err != nil {
				return nil, fmt.Errorf("provider %q: %w", c.Name, err)
			}
			key = v
		}
		// Normalise the base URL — strip trailing slashes so we can
		// always append /chat/completions without doubling up.
		base := strings.TrimRight(c.BaseURL, "/")
		if _, err := url.Parse(base); err != nil {
			return nil, fmt.Errorf("provider %q: invalid baseUrl %q", c.Name, c.BaseURL)
		}
		r.byName[c.Name] = &Provider{
			Name:      c.Name,
			BaseURL:   base,
			APIKey:    key,
			AuthStyle: defaultStr(c.AuthStyle, "bearer"),
			Headers:   c.Headers,
		}
		r.order = append(r.order, c.Name)
	}
	return r, nil
}

// Pick parses an incoming model string and resolves the upstream. The
// returned downstreamModel is what should be sent to the upstream in
// the request body's `model` field — i.e. with the "<provider>/" prefix
// stripped.
func (r *Router) Pick(model string) (p *Provider, downstreamModel string, err error) {
	name, rest, ok := strings.Cut(model, "/")
	if !ok {
		return nil, "", fmt.Errorf("model %q must be in form <provider>/<model>", model)
	}
	pv, ok := r.byName[name]
	if !ok {
		return nil, "", fmt.Errorf("unknown provider %q", name)
	}
	return pv, rest, nil
}

// ListProviders returns the registered provider names in declaration
// order. /v1/models uses this to build its catalog.
func (r *Router) ListProviders() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Get returns a provider by name, or nil if unknown. Useful for tests
// and admin diagnostics.
func (r *Router) Get(name string) *Provider {
	return r.byName[name]
}

func defaultStr(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
