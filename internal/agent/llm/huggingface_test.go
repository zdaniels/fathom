package llm

import (
	"net/url"
	"strings"
	"testing"

	"github.com/zdaniels/fathom/pkg/types"
)

// HuggingFace is wired as an OpenAI-compatible delegate pointed at the HF
// Inference Providers router. These assert the delegation defaults the way
// DeepSeek/Groq/etc. are wired.
func TestHuggingFaceProviderDefaults(t *testing.T) {
	lookup := func(name string) (string, error) {
		if name != "HF_TOKEN" {
			t.Errorf("expected HF_TOKEN lookup, got %q", name)
		}
		return "hf_secret", nil
	}
	for _, name := range []string{"huggingface", "hf"} {
		p, err := New(types.LLMConfig{Provider: name, Model: "deepseek-ai/DeepSeek-R1"}, lookup)
		if err != nil {
			t.Fatalf("%s: New errored: %v", name, err)
		}
		oa, ok := p.(*OpenAI)
		if !ok {
			t.Fatalf("%s: expected *OpenAI delegate, got %T", name, p)
		}
		if oa.baseURL != "https://router.huggingface.co/v1" {
			t.Errorf("%s: baseURL = %q, want the HF router", name, oa.baseURL)
		}
		if oa.apiKey != "hf_secret" {
			t.Errorf("%s: apiKey not threaded through", name)
		}
	}
}

func TestHuggingFaceRespectsCustomBaseURL(t *testing.T) {
	// A user pointing at a self-hosted endpoint (e.g. vLLM serving Mellum-2)
	// keeps their BaseURL — withDefaultBase only fills when empty.
	p, err := New(types.LLMConfig{
		Provider: "huggingface",
		Model:    "JetBrains/Mellum2-12B-A2.5B-Instruct",
		BaseURL:  "http://localhost:8000/v1",
	}, func(string) (string, error) { return "k", nil })
	if err != nil {
		t.Fatal(err)
	}
	if oa := p.(*OpenAI); oa.baseURL != "http://localhost:8000/v1" {
		t.Errorf("custom baseURL overridden: got %q", oa.baseURL)
	}
}

func TestHuggingFaceMissingTokenErrors(t *testing.T) {
	_, err := New(types.LLMConfig{Provider: "hf", Model: "x"},
		func(string) (string, error) { return "", &ProviderError{Provider: "vault", Msg: "not found"} })
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing HF_TOKEN should surface the lookup error, got: %v", err)
	}
}

// Guard against the class of bug where a default base URL lost its TLD
// (e.g. "https://openrouter/api/v1" — no dot, DNS can't resolve). Every
// OpenAI-delegated cloud provider must resolve to an absolute https URL whose
// host contains a dot. Catches stripped domains before they ship.
func TestDelegatedProviderBaseURLsAreValid(t *testing.T) {
	lookup := func(string) (string, error) { return "key", nil }
	cases := []struct {
		provider string
		wantHost string
	}{
		{"openai", "api.openai.com"},
		{"deepseek", "api.deepseek.com"},
		{"xai", "api.x.ai"},
		{"xiaomi", "api.xiaomimimo.com"},
		{"groq", "api.groq.com"},
		{"openrouter", "openrouter.ai"},
		{"together", "api.together.ai"},
		{"fireworks", "api.fireworks.ai"},
		{"mistral", "api.mistral.ai"},
		{"huggingface", "router.huggingface.co"},
	}
	for _, c := range cases {
		p, err := New(types.LLMConfig{Provider: c.provider, Model: "m"}, lookup)
		if err != nil {
			t.Errorf("%s: New errored: %v", c.provider, err)
			continue
		}
		oa, ok := p.(*OpenAI)
		if !ok {
			t.Errorf("%s: not an OpenAI delegate (%T)", c.provider, p)
			continue
		}
		u, err := url.Parse(oa.baseURL)
		if err != nil {
			t.Errorf("%s: baseURL %q does not parse: %v", c.provider, oa.baseURL, err)
			continue
		}
		if u.Scheme != "https" {
			t.Errorf("%s: baseURL scheme = %q, want https", c.provider, u.Scheme)
		}
		if !strings.Contains(u.Host, ".") {
			t.Errorf("%s: baseURL host %q has no TLD — likely a stripped domain", c.provider, u.Host)
		}
		if u.Host != c.wantHost {
			t.Errorf("%s: baseURL host = %q, want %q", c.provider, u.Host, c.wantHost)
		}
	}
}
