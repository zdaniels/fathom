package router

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zdaniels/fathom/internal/agent/llm"
	"github.com/zdaniels/fathom/pkg/types"
)

// stubProvider lets a test specify exactly what the classifier "model"
// will reply with for the next Chat call. Captures the messages it was
// given so prompt-shape assertions can run against it.
type stubProvider struct {
	reply    string
	err      error
	captured []llm.Message
}

func (s *stubProvider) Chat(_ context.Context, messages []llm.Message, _ []llm.ToolDef) (llm.Response, error) {
	s.captured = messages
	if s.err != nil {
		return llm.Response{}, s.err
	}
	return llm.Response{Content: s.reply, FinishReason: llm.FinishStop}, nil
}

// newWithStub constructs a Router whose classifier is a stubProvider —
// keeps tests off the network. We bypass New() so we can inject the
// provider directly instead of going through llm.New() with secrets.
func newWithStub(t *testing.T, profiles []string, fallback string, reply string) (*Router, *stubProvider) {
	t.Helper()
	stub := &stubProvider{reply: reply}
	r := &Router{
		classifier:   stub,
		classifierID: "stub",
		profileNames: append([]string{}, profiles...),
		fallback:     fallback,
		timeout:      2 * time.Second,
	}
	// renderDescriptions normally happens in New — synthesise a minimal
	// catalog here so systemPrompt() has something to interpolate.
	pmap := map[string]types.ProfileConfig{}
	for _, p := range profiles {
		pmap[p] = types.ProfileConfig{Categories: []string{p}}
	}
	r.descriptions = renderDescriptions(r.profileNames, pmap, nil)
	return r, stub
}

func TestClassify_ExactMatch(t *testing.T) {
	r, _ := newWithStub(t, []string{"general", "code", "communication"}, "general", "communication")
	if got := r.Classify(context.Background(), "send a slack message"); got != "communication" {
		t.Fatalf("want communication, got %q", got)
	}
}

func TestClassify_HandlesPunctuationWrap(t *testing.T) {
	// Small models often disobey the "one word, no punctuation" rule.
	cases := map[string]string{
		"communication.":          "communication",
		"  communication  ":       "communication",
		"`communication`":         "communication",
		"\"communication\"":       "communication",
		"Category: communication": "communication",
		"communication\n":         "communication",
	}
	for input, want := range cases {
		r, _ := newWithStub(t, []string{"general", "code", "communication"}, "general", input)
		if got := r.Classify(context.Background(), "x"); got != want {
			t.Errorf("input %q: want %q, got %q", input, want, got)
		}
	}
}

func TestClassify_CaseInsensitive(t *testing.T) {
	r, _ := newWithStub(t, []string{"general", "Communication"}, "general", "COMMUNICATION")
	if got := r.Classify(context.Background(), "x"); got != "Communication" {
		t.Fatalf("want Communication (preserved case from config), got %q", got)
	}
}

func TestClassify_FallbackOnUnknownCategory(t *testing.T) {
	r, _ := newWithStub(t, []string{"general", "code"}, "general", "billing")
	if got := r.Classify(context.Background(), "what's my bill"); got != "general" {
		t.Fatalf("unknown category should fall back to 'general', got %q", got)
	}
}

func TestClassify_FallbackOnProviderError(t *testing.T) {
	r, stub := newWithStub(t, []string{"general", "code"}, "general", "")
	stub.err = errors.New("ollama 500")
	if got := r.Classify(context.Background(), "x"); got != "general" {
		t.Fatalf("provider error should fall back to 'general', got %q", got)
	}
}

func TestClassify_FallbackOnEmptyReply(t *testing.T) {
	r, _ := newWithStub(t, []string{"general", "code"}, "general", "   ")
	if got := r.Classify(context.Background(), "x"); got != "general" {
		t.Fatalf("empty reply should fall back, got %q", got)
	}
}

func TestClassify_FallbackOnSubstringFalsePositive(t *testing.T) {
	// "generic" should NOT match "general" — word-boundary parsing only.
	r, _ := newWithStub(t, []string{"general", "code"}, "general", "generic_response")
	got := r.Classify(context.Background(), "x")
	// Either matches nothing → fallback, or matches general because the
	// classifier's reply tokenises. The contract is "fall back if not a
	// known whole word"; "generic" tokenises to ["generic","response"],
	// neither is "general", so fall back.
	if got != "general" {
		t.Fatalf("substring 'generic' should not match 'general'; want fallback, got %q", got)
	}
}

func TestSystemPrompt_ListsAllProfiles(t *testing.T) {
	r, _ := newWithStub(t, []string{"alpha", "beta", "gamma"}, "alpha", "")
	prompt := r.systemPrompt()
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(prompt, "- "+name+":") {
			t.Errorf("system prompt missing profile line for %q\nprompt:\n%s", name, prompt)
		}
	}
	if !strings.Contains(prompt, "If you are unsure, reply with: alpha") {
		t.Errorf("system prompt missing fallback hint:\n%s", prompt)
	}
}

func TestClassify_PassesUserMessageThrough(t *testing.T) {
	r, stub := newWithStub(t, []string{"a", "b"}, "a", "a")
	r.Classify(context.Background(), "hello world")
	if len(stub.captured) != 2 {
		t.Fatalf("want 2 messages (system + user), got %d", len(stub.captured))
	}
	if stub.captured[0].Role != "system" {
		t.Fatalf("first message should be system, got %q", stub.captured[0].Role)
	}
	if stub.captured[1].Role != "user" || stub.captured[1].Content != "hello world" {
		t.Fatalf("user message not forwarded: %+v", stub.captured[1])
	}
}

func TestNew_ValidatesConfig(t *testing.T) {
	t.Run("nil when no profiles", func(t *testing.T) {
		r, err := New(Options{
			Config:    types.RoutingConfig{},
			LLMConfig: types.LLMConfig{},
		})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if r != nil {
			t.Fatal("expected nil router when no profiles")
		}
	})
	t.Run("rejects missing router model", func(t *testing.T) {
		_, err := New(Options{
			Config: types.RoutingConfig{
				Profiles: map[string]types.ProfileConfig{
					"a": {Categories: []string{"x"}},
				},
			},
			LLMConfig: types.LLMConfig{},
		})
		if err == nil || !strings.Contains(err.Error(), "router.model") {
			t.Fatalf("want router.model error, got %v", err)
		}
	})
	t.Run("rejects unknown router model", func(t *testing.T) {
		_, err := New(Options{
			Config: types.RoutingConfig{
				Router: types.RouterConfig{Model: "unknown"},
				Profiles: map[string]types.ProfileConfig{
					"a": {Categories: []string{"x"}},
				},
			},
			LLMConfig: types.LLMConfig{
				Models: map[string]types.LLMModelConfig{
					"other": {Provider: "ollama", Model: "x"},
				},
			},
		})
		if err == nil || !strings.Contains(err.Error(), "not found in llm.models") {
			t.Fatalf("want llm.models error, got %v", err)
		}
	})
	t.Run("rejects unknown fallback profile", func(t *testing.T) {
		_, err := New(Options{
			Config: types.RoutingConfig{
				Router: types.RouterConfig{Model: "classifier", Fallback: "ghost"},
				Profiles: map[string]types.ProfileConfig{
					"a": {Categories: []string{"x"}},
				},
			},
			LLMConfig: types.LLMConfig{
				Models: map[string]types.LLMModelConfig{
					"classifier": {Provider: "ollama", Model: "qwen2.5:0.5b"},
				},
			},
		})
		if err == nil || !strings.Contains(err.Error(), "fallback") {
			t.Fatalf("want fallback error, got %v", err)
		}
	})
}

func TestRenderDescriptions_UsesCustomWhenProvided(t *testing.T) {
	pmap := map[string]types.ProfileConfig{
		"email": {Skills: []string{"gmail"}},
		"code":  {Categories: []string{"development"}},
	}
	custom := map[string]string{"email": "All email-related requests"}
	out := renderDescriptions([]string{"code", "email"}, pmap, custom)
	if !strings.Contains(out, "- email: All email-related requests") {
		t.Errorf("custom description not honoured:\n%s", out)
	}
	if !strings.Contains(out, "- code: category: development") {
		t.Errorf("synthesised description missing for code:\n%s", out)
	}
}

func TestSynthesiseDescription(t *testing.T) {
	cases := []struct {
		name   string
		input  types.ProfileConfig
		expect string
	}{
		{"empty", types.ProfileConfig{}, "general-purpose"},
		{"skills only", types.ProfileConfig{Skills: []string{"a", "b"}}, "skills: a, b"},
		{"categories only", types.ProfileConfig{Categories: []string{"x"}}, "category: x"},
		{"both", types.ProfileConfig{Skills: []string{"a"}, Categories: []string{"x"}}, "category: x; skills: a"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := synthesiseDescription(c.input); got != c.expect {
				t.Errorf("want %q, got %q", c.expect, got)
			}
		})
	}
}
