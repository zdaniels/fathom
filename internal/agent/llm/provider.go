// Package llm holds the LLM provider implementations Fathom calls into.
// Each provider satisfies Provider with the same interface — Chat(messages,
// tools) → Response — so the agent loop is provider-agnostic.
package llm

import (
	"context"
	"fmt"
	"os"

	"github.com/zdaniels/fathom/pkg/types"
)

// Message is a single turn in the conversation. Mirrors the OpenAI-style
// {role, content, name, tool_call_id} shape; provider implementations
// translate to their wire format.
type Message struct {
	Role       string // "system" | "user" | "assistant" | "tool"
	Content    string
	Name       string // for "tool" role
	ToolCallID string // for "tool" role
	// ToolCalls is set on assistant messages that requested tools. It MUST
	// be carried back in subsequent turns or the model loses track of which
	// call each tool result corresponds to and either errors out or loops.
	ToolCalls []ToolCallRequest
	Trusted   bool // false when the content originates from a third party
}

// ToolDef is the LLM-facing tool description. JSON-Schema-ish parameters.
type ToolDef struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// ToolCallRequest is what the model emits when it wants to call a tool.
type ToolCallRequest struct {
	ID        string                 `json:"id"`
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

// Usage tracks token consumption for accounting / rate-limit decisions.
type Usage struct {
	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
}

// FinishReason categorises why the model stopped generating.
type FinishReason string

const (
	FinishStop      FinishReason = "stop"
	FinishToolCalls FinishReason = "tool_calls"
	FinishLength    FinishReason = "length"
	FinishError     FinishReason = "error"
)

// Response is the unified shape Fathom expects from every provider.
type Response struct {
	Content      string
	ToolCalls    []ToolCallRequest
	Usage        *Usage
	FinishReason FinishReason
}

// Provider is the agent loop's contract with any backing LLM. Implementations
// hand-roll the HTTP layer rather than pulling in vendor SDKs — keeps the
// dependency footprint tiny and SDK drift out of the picture.
type Provider interface {
	Chat(ctx context.Context, messages []Message, tools []ToolDef) (Response, error)
}

// SecretLookup is how providers fetch their API key without ever holding it
// statically. Same shape as the runtime's resolver — vault first, then env.
type SecretLookup func(name string) (string, error)

// New constructs the right provider for the configured LLMConfig. Returns
// an error if the provider is unknown or the required secret is missing.
func New(cfg types.LLMConfig, lookup SecretLookup) (Provider, error) {
	originalLookup := lookup
	lookup = func(name string) (string, error) {
		key, err := originalLookup(name)
		if err != nil || key == "" {
			return "", &MissingCredentialError{Name: name, Err: err}
		}
		return key, nil
	}
	switch cfg.Provider {
	case "openai":
		key, err := lookup("OPENAI_API_KEY")
		if err != nil {
			return nil, err
		}
		return NewOpenAI(cfg, key), nil
	case "anthropic":
		key, err := lookup("ANTHROPIC_API_KEY")
		if err != nil {
			return nil, err
		}
		return NewAnthropic(cfg, key), nil
	case "ollama":
		return NewOllama(cfg), nil
	case "gemini":
		key, err := lookup("GEMINI_API_KEY")
		if err != nil {
			return nil, err
		}
		return NewGemini(cfg, key), nil
	case "bedrock":
		ak, err := lookup("AWS_ACCESS_KEY_ID")
		if err != nil {
			return nil, err
		}
		sk, err := lookup("AWS_SECRET_ACCESS_KEY")
		if err != nil {
			return nil, err
		}
		// Session token is optional — only present for STS / IAM-role
		// temporary credentials. Missing is fine; the lookup errors are
		// swallowed so static-creds setups don't have to set it.
		st, _ := lookup("AWS_SESSION_TOKEN")
		// Region resolution: cfg.BaseURL is reused as the region holder
		// when it doesn't look like a URL (Bedrock is region-scoped, not
		// endpoint-scoped). Falls back to env, then us-east-1.
		region := cfg.BaseURL
		if region == "" || looksLikeURL(region) {
			region = os.Getenv("AWS_REGION")
			if region == "" {
				region = os.Getenv("AWS_DEFAULT_REGION")
			}
		}
		return NewBedrock(cfg, region, ak, sk, st), nil
	case "deepseek":
		// DeepSeek serves an OpenAI-compatible /chat/completions API with
		// tool calling — same wire format as openai, different baseURL +
		// API key. Delegate to the openai client with the right defaults.
		key, err := lookup("DEEPSEEK_API_KEY")
		if err != nil {
			return nil, err
		}
		return NewOpenAI(withDefaultBase(cfg, "https://api.deepseek.com/v1"), key), nil
	case "xai":
		// xAI / Grok also exposes an OpenAI-compatible chat completions
		// API. Same delegation pattern as DeepSeek.
		key, err := lookup("XAI_API_KEY")
		if err != nil {
			return nil, err
		}
		return NewOpenAI(withDefaultBase(cfg, "https://api.x.ai/v1"), key), nil
	case "xiaomi", "mimo":
		// Xiaomi's MiMo API platform (api.xiaomimimo.com) is
		// OpenAI-compatible — bearer auth, /chat/completions shape.
		// Accepting "mimo" as an alias because that's how Xiaomi brands
		// the API in their own docs.
		key, err := lookup("XIAOMI_MIMO_API_KEY")
		if err != nil {
			return nil, err
		}
		return NewOpenAI(withDefaultBase(cfg, "https://api.xiaomimimo.com/v1"), key), nil
	case "groq":
		// Groq's LPU-backed inference — OpenAI-compatible at
		// api.groq.com/openai/v1 (note the /openai/ path segment).
		key, err := lookup("GROQ_API_KEY")
		if err != nil {
			return nil, err
		}
		return NewOpenAI(withDefaultBase(cfg, "https://api.groq.com/openai/v1"), key), nil
	case "openrouter":
		// OpenRouter is a model aggregator — same wire format as OpenAI,
		// but the `model` field uses provider-prefixed slugs like
		// "anthropic/claude-sonnet-latest" or "meta-llama/llama-3.3-70b".
		key, err := lookup("OPENROUTER_API_KEY")
		if err != nil {
			return nil, err
		}
		return NewOpenAI(withDefaultBase(cfg, "https://openrouter.ai/api/v1"), key), nil
	case "together":
		// Together AI — hosts Llama / Mistral / Qwen / DeepSeek / etc.
		// behind an OpenAI-compatible API.
		key, err := lookup("TOGETHER_API_KEY")
		if err != nil {
			return nil, err
		}
		return NewOpenAI(withDefaultBase(cfg, "https://api.together.ai/v1"), key), nil
	case "fireworks":
		// Fireworks AI — same shape; model identifiers follow the form
		// "accounts/fireworks/models/<slug>" (or your own account's
		// fine-tuned variants).
		key, err := lookup("FIREWORKS_API_KEY")
		if err != nil {
			return nil, err
		}
		return NewOpenAI(withDefaultBase(cfg, "https://api.fireworks.ai/inference/v1"), key), nil
	case "mistral":
		// Mistral La Plateforme — OpenAI-compatible /chat/completions
		// with tool-calling. Use the "-latest" suffix for model strings
		// to ride forward on new releases without config churn.
		key, err := lookup("MISTRAL_API_KEY")
		if err != nil {
			return nil, err
		}
		return NewOpenAI(withDefaultBase(cfg, "https://api.mistral.ai/v1"), key), nil
	case "lmstudio":
		// LM Studio runs a local OpenAI-compatible server (default
		// http://localhost:1234/v1). No key required; the bearer header
		// is sent with an empty value and LM Studio ignores it.
		return NewOpenAI(withDefaultBase(cfg, "http://localhost:1234/v1"), ""), nil
	case "huggingface", "hf":
		// HuggingFace Inference Providers expose a drop-in OpenAI-compatible
		// router at router.huggingface.co/v1 (bearer HF_TOKEN). The `model`
		// field uses the Hub repo id, optionally with a provider/policy
		// suffix, e.g. "deepseek-ai/DeepSeek-R1", "openai/gpt-oss-120b:cheapest",
		// or "...:sambanova". Models not on the serverless router (e.g.
		// self-hosted Mellum-2 via vLLM) won't resolve here — point cfg.BaseURL
		// at your own OpenAI-compatible endpoint and use provider "openai" or
		// "lmstudio" instead. "hf" is accepted as a shorthand alias.
		key, err := lookup("HF_TOKEN")
		if err != nil {
			return nil, err
		}
		return NewOpenAI(withDefaultBase(cfg, "https://router.huggingface.co/v1"), key), nil
	default:
		return nil, &ProviderError{Provider: cfg.Provider, Msg: "unknown provider"}
	}
}

// withDefaultBase fills in cfg.BaseURL only when it's empty, so users can
// still override the endpoint per-provider (e.g. point at a regional
// DeepSeek mirror or a self-hosted xAI gateway) without losing the
// first-class provider name.
func withDefaultBase(cfg types.LLMConfig, base string) types.LLMConfig {
	if cfg.BaseURL == "" {
		cfg.BaseURL = base
	}
	return cfg
}

func looksLikeURL(s string) bool {
	return len(s) >= 4 && s[:4] == "http"
}

// ProviderError wraps provider-specific errors with context for the agent
// loop's error surface.
type ProviderError struct {
	Provider string
	Msg      string
	Err      error
}

func (e *ProviderError) Error() string {
	if e.Err != nil {
		return e.Provider + ": " + e.Msg + ": " + e.Err.Error()
	}
	return e.Provider + ": " + e.Msg
}

func (e *ProviderError) Unwrap() error { return e.Err }

// MissingCredentialError distinguishes incomplete setup from invalid configuration.
type MissingCredentialError struct {
	Name string
	Err  error
}

func (e *MissingCredentialError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("missing credential %s: %v", e.Name, e.Err)
	}
	return fmt.Sprintf("missing credential %s", e.Name)
}

func (e *MissingCredentialError) Unwrap() error { return e.Err }
