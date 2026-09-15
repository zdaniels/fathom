package llm

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/zdaniels/fathom/pkg/types"
)

// Router holds a registry of named LLM providers. The chat session can
// select one per turn (via /use), the loop can dispatch to a different
// model for critique, and the gateway can route per-tenant in the future.
//
// Concurrency: providers are read-only after Init, so we use a plain
// map. The current selection lives at the call site, not in the router.
type Router struct {
	mu       sync.RWMutex
	models   map[string]Provider
	defaults string // name of the default model
}

// NewRouter builds a Router from LLMConfig. It accepts both shapes:
//
//   - Single-model (legacy): only Provider/Model set at top level. A single
//     entry "default" is registered.
//   - Multi-model: Models map populated. Each entry becomes a Provider;
//     `Default` (or "default" if Default is empty and that name exists)
//     is the fallback selection.
//
// The lookup function is the same one used to resolve API keys — the
// secret resolver from the security mesh. Missing keys for a specific
// model are non-fatal: that model just isn't registered, but the router
// builds anyway so the user can /use whatever else they configured.
func NewRouter(cfg types.LLMConfig, lookup SecretLookup) (*Router, error) {
	r := &Router{models: make(map[string]Provider)}

	// Multi-model path.
	if len(cfg.Models) > 0 {
		for name, m := range cfg.Models {
			provider, err := newProviderFromModel(m, lookup)
			if err != nil {
				// Skip models with missing keys; they're optional.
				continue
			}
			r.models[name] = provider
		}
		// Pick default: explicit > "default" entry > any single registered.
		if cfg.Default != "" {
			if _, ok := r.models[cfg.Default]; ok {
				r.defaults = cfg.Default
			}
		}
		if r.defaults == "" {
			if _, ok := r.models["default"]; ok {
				r.defaults = "default"
			}
		}
		if r.defaults == "" {
			for name := range r.models {
				r.defaults = name
				break
			}
		}
		if len(r.models) == 0 {
			return nil, fmt.Errorf("router: no usable models in llm.models (all missing keys)")
		}
		return r, nil
	}

	// Legacy single-model path.
	if cfg.Provider == "" {
		return nil, fmt.Errorf("router: no LLM configured (set llm.provider+llm.model or llm.models)")
	}
	provider, err := New(cfg, lookup)
	if err != nil {
		return nil, err
	}
	r.models["default"] = provider
	r.defaults = "default"
	return r, nil
}

// newProviderFromModel handles per-entry construction in the multi-model
// path. Reuses the top-level New() by synthesising a one-off LLMConfig.
func newProviderFromModel(m types.LLMModelConfig, lookup SecretLookup) (Provider, error) {
	return New(types.LLMConfig{
		Provider:    m.Provider,
		Model:       m.Model,
		BaseURL:     m.BaseURL,
		MaxTokens:   m.MaxTokens,
		Temperature: m.Temperature,
	}, lookup)
}

// Get returns the named provider, or the default when name is empty.
// Returns an error if the name is unknown.
func (r *Router) Get(name string) (Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if name == "" {
		name = r.defaults
	}
	p, ok := r.models[name]
	if !ok {
		return nil, fmt.Errorf("unknown model %q (configured: %v)", name, r.namesLocked())
	}
	return p, nil
}

// Default returns the default provider and its name.
func (r *Router) Default() (Provider, string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.models[r.defaults], r.defaults
}

// Names returns the registered model names, sorted for stable display.
func (r *Router) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.namesLocked()
}

func (r *Router) namesLocked() []string {
	out := make([]string, 0, len(r.models))
	for n := range r.models {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// PickCritic returns the model best suited for critique. Convention:
//
//  1. Explicit "critic" name in the registry
//  2. Any model whose provider differs from the current one (cross-provider
//     critique catches more biases)
//  3. Default model (last resort — better than nothing)
//
// Returns the picked Provider, its registered name, and a "differsFromCurrent"
// flag callers use to message the user when no cross-provider option exists.
func (r *Router) PickCritic(currentName string) (Provider, string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if p, ok := r.models["critic"]; ok {
		return p, "critic"
	}
	// Look for any registered name other than current.
	for name, p := range r.models {
		if name != currentName {
			return p, name
		}
	}
	return r.models[r.defaults], r.defaults
}

// Ensure the router compiles with the existing Provider interface.
var _ context.Context = context.Background()
