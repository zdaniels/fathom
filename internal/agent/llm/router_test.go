package llm

import (
	"strings"
	"testing"

	"github.com/zdaniels/fathom/pkg/types"
)

func TestRouterLegacySingleModel(t *testing.T) {
	cfg := types.LLMConfig{Provider: "ollama", Model: "qwen3"}
	r, err := NewRouter(cfg, func(string) (string, error) { return "", nil })
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if names := r.Names(); len(names) != 1 || names[0] != "default" {
		t.Errorf("legacy mode should register one 'default' entry, got %v", names)
	}
	if _, name := r.Default(); name != "default" {
		t.Errorf("Default = %q, want 'default'", name)
	}
}

func TestRouterMultiModelRegistersAll(t *testing.T) {
	cfg := types.LLMConfig{
		Default: "local",
		Models: map[string]types.LLMModelConfig{
			"local":  {Provider: "ollama", Model: "qwen3"},
			"coding": {Provider: "ollama", Model: "qwen3:32b"}, // no API key needed for ollama
		},
	}
	r, err := NewRouter(cfg, func(string) (string, error) { return "fake-key", nil })
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if names := r.Names(); len(names) != 2 {
		t.Errorf("registered = %v, want 2 entries", names)
	}
	if _, name := r.Default(); name != "local" {
		t.Errorf("default = %q, want 'local'", name)
	}
}

func TestRouterPickCriticPrefersExplicitName(t *testing.T) {
	cfg := types.LLMConfig{
		Default: "primary",
		Models: map[string]types.LLMModelConfig{
			"primary": {Provider: "ollama", Model: "qwen3"},
			"critic":  {Provider: "ollama", Model: "llama3"},
			"other":   {Provider: "ollama", Model: "mistral"},
		},
	}
	r, _ := NewRouter(cfg, func(string) (string, error) { return "", nil })
	_, name := r.PickCritic("primary")
	if name != "critic" {
		t.Errorf("PickCritic = %q, want 'critic'", name)
	}
}

func TestRouterPickCriticFallsBackToCrossProvider(t *testing.T) {
	cfg := types.LLMConfig{
		Default: "a",
		Models: map[string]types.LLMModelConfig{
			"a": {Provider: "ollama", Model: "qwen3"},
			"b": {Provider: "ollama", Model: "llama3"},
		},
	}
	r, _ := NewRouter(cfg, func(string) (string, error) { return "", nil })
	_, name := r.PickCritic("a")
	if name == "a" {
		t.Errorf("PickCritic should not return the current model")
	}
}

func TestRouterGetUnknownNameErrors(t *testing.T) {
	cfg := types.LLMConfig{
		Models: map[string]types.LLMModelConfig{
			"a": {Provider: "ollama", Model: "qwen3"},
		},
	}
	r, _ := NewRouter(cfg, func(string) (string, error) { return "", nil })
	_, err := r.Get("nope")
	if err == nil || !strings.Contains(err.Error(), "unknown model") {
		t.Errorf("Get(unknown) should error, got %v", err)
	}
}

func TestRouterMissingProviderErrors(t *testing.T) {
	cfg := types.LLMConfig{} // nothing set
	_, err := NewRouter(cfg, func(string) (string, error) { return "", nil })
	if err == nil {
		t.Error("empty config should error")
	}
}

func TestRouterSkipsMissingKeyEntries(t *testing.T) {
	// Anthropic needs an API key; the lookup returns an error → entry is
	// skipped but the router still builds.
	cfg := types.LLMConfig{
		Models: map[string]types.LLMModelConfig{
			"local": {Provider: "ollama", Model: "qwen3"},
			"cloud": {Provider: "anthropic", Model: "claude-sonnet-4-5"},
		},
	}
	r, err := NewRouter(cfg, func(name string) (string, error) {
		if name == "ANTHROPIC_API_KEY" {
			return "", errAPIKeyMissing
		}
		return "", nil
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	names := r.Names()
	if len(names) != 1 || names[0] != "local" {
		t.Errorf("expected only 'local' (cloud has no key), got %v", names)
	}
}

var errAPIKeyMissing = &ProviderError{Provider: "anthropic", Msg: "missing key"}
