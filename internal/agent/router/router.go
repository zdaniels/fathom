// Package router implements intent-based agent routing.
//
// On every user message, a small fast classifier LLM picks one of N
// configured profiles. The picked profile decides which downstream model
// answers AND which subset of skills/tools that model sees in its context.
// This keeps prompt context small per request — critical for smaller local
// models, where 30+ tools in the prompt makes them slow and unreliable at
// tool calling.
//
// The classifier itself is a single short LLM call (~200ms with a 0.5b–1.5b
// model). Its prompt is hardcoded here — only the model name is config-
// driven. We deliberately don't let users customise the classifier prompt
// because if their classifier text drifts from the profile descriptions
// the whole routing layer becomes flaky and hard to debug.
//
// If anything fails — model error, malformed reply, unknown category —
// the router quietly falls back to RouterConfig.Fallback (or the first
// profile in lexicographic order if Fallback is empty). Routing should
// never block a request from reaching some agent.
package router

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/zdaniels/fathom/internal/agent/llm"
	"github.com/zdaniels/fathom/pkg/types"
)

// Router classifies a user message into one of its configured profiles.
//
// Construct once via New() and reuse across requests — the classifier
// LLM client is held internally and Classify() is safe for concurrent
// use. The profile descriptions list is built at construction time so
// adding a profile requires a Router restart (matches how the agent
// loop loads tools).
type Router struct {
	classifier   llm.Provider
	classifierID string // model name, for log lines
	profileNames []string
	fallback     string
	descriptions string // pre-rendered for the classifier system prompt
	timeout      time.Duration
}

// Options bundles what New() needs.
type Options struct {
	Config types.RoutingConfig
	// LLMConfig is the parent llm: block — used to resolve the classifier's
	// model entry by name. The classifier MUST appear in cfg.LLM.Models.
	LLMConfig types.LLMConfig
	// Lookup retrieves secrets (API keys) for the classifier provider. Same
	// shape as agent factory uses.
	Lookup llm.SecretLookup
	// ProfileDescriptions is an optional map of profile-name → human-
	// readable description that the classifier uses to decide.
	// Defaults are inferred from profile names if a description is missing.
	ProfileDescriptions map[string]string
	// Timeout caps each classification call. 5s is plenty for tiny models.
	Timeout time.Duration
}

// classifierTimeoutDefault is the cap on each classify() round-trip.
// 5 seconds is generous for a 0.5b–1.5b model; we'd rather fall back
// than hold the user's whole request waiting on the classifier.
const classifierTimeoutDefault = 5 * time.Second

// New builds a Router. Returns nil, nil when opts.Config has no profiles
// — callers should treat that as "routing disabled, use the single-agent
// path" rather than as an error.
func New(opts Options) (*Router, error) {
	if len(opts.Config.Profiles) == 0 {
		return nil, nil
	}
	if opts.Config.Router.Model == "" {
		return nil, fmt.Errorf("routing.router.model is required when profiles are defined")
	}
	modelCfg, ok := opts.LLMConfig.Models[opts.Config.Router.Model]
	if !ok {
		return nil, fmt.Errorf("routing.router.model %q not found in llm.models", opts.Config.Router.Model)
	}

	// Build a one-off LLMConfig that uses the named entry. We don't want
	// the classifier to inherit MaxTokens/Temperature from the default
	// agent — those are tuned for thinking-and-answering, not for emitting
	// a single category token.
	cfg := types.LLMConfig{
		Provider:    modelCfg.Provider,
		Model:       modelCfg.Model,
		BaseURL:     modelCfg.BaseURL,
		Temperature: 0,  // deterministic classification
		MaxTokens:   32, // we just need the category name back
	}
	prov, err := llm.New(cfg, opts.Lookup)
	if err != nil {
		return nil, fmt.Errorf("build classifier provider: %w", err)
	}

	// Sort profile names so the same input always renders the same prompt
	// (helps with prompt caching at the provider level + golden tests).
	names := make([]string, 0, len(opts.Config.Profiles))
	for n := range opts.Config.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)

	descriptions := renderDescriptions(names, opts.Config.Profiles, opts.ProfileDescriptions)

	fallback := opts.Config.Router.Fallback
	if fallback == "" {
		fallback = names[0] // first alphabetically when unset
	}
	if _, ok := opts.Config.Profiles[fallback]; !ok {
		return nil, fmt.Errorf("routing.router.fallback %q is not a defined profile", fallback)
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = classifierTimeoutDefault
	}

	return &Router{
		classifier:   prov,
		classifierID: opts.Config.Router.Model,
		profileNames: names,
		fallback:     fallback,
		descriptions: descriptions,
		timeout:      timeout,
	}, nil
}

// Classify picks the most appropriate profile for the given user message.
// Returns the profile name. Never returns an error — on any failure it
// logs and falls back to r.fallback so the request keeps moving.
func (r *Router) Classify(ctx context.Context, message string) string {
	if r == nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	resp, err := r.classifier.Chat(cctx, []llm.Message{
		{Role: "system", Content: r.systemPrompt()},
		{Role: "user", Content: message},
	}, nil)
	if err != nil {
		slog.Warn("router: classify failed; using fallback",
			"err", err, "fallback", r.fallback, "model", r.classifierID)
		return r.fallback
	}
	picked := r.parse(resp.Content)
	if picked == "" {
		slog.Warn("router: classifier reply did not match a known profile; using fallback",
			"reply", truncate(resp.Content, 80), "fallback", r.fallback)
		return r.fallback
	}
	slog.Debug("router: classified", "profile", picked, "model", r.classifierID)
	return picked
}

// systemPrompt assembles the classifier's instructions. Kept deliberately
// terse so a 0.5b–1.5b model can follow it. Output a SINGLE word.
func (r *Router) systemPrompt() string {
	var b strings.Builder
	b.WriteString("You are a router. Classify the user's message into exactly ONE of these categories:\n\n")
	b.WriteString(r.descriptions)
	b.WriteString("\n")
	b.WriteString("Rules:\n")
	b.WriteString("- Reply with ONLY the category name (one word). No explanation, no punctuation, no quotes.\n")
	b.WriteString("- If the message could fit multiple categories, pick the most specific one.\n")
	b.WriteString("- If you are unsure, reply with: ")
	b.WriteString(r.fallback)
	return b.String()
}

// parse extracts a known profile name from the classifier's response. We
// accept anything that contains a profile name as a whole word — small
// models sometimes wrap the answer in punctuation or add "category:"
// despite the system prompt.
func (r *Router) parse(content string) string {
	c := strings.ToLower(strings.TrimSpace(content))
	c = strings.Trim(c, ".,:;!\"'`()[]{}")
	if c == "" {
		return ""
	}
	// Exact match first — most reliable.
	for _, name := range r.profileNames {
		if c == strings.ToLower(name) {
			return name
		}
	}
	// Word-boundary contains — handles "Category: communication" etc.
	for _, name := range r.profileNames {
		lower := strings.ToLower(name)
		// avoid substring false positives like "general" matching "generic"
		for _, tok := range strings.FieldsFunc(c, func(r rune) bool {
			return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' && r != '_'
		}) {
			if tok == lower {
				return name
			}
		}
	}
	return ""
}

// ProfileNames returns the configured profile names in sorted order.
// Exposed mostly for tests + diagnostics.
func (r *Router) ProfileNames() []string {
	if r == nil {
		return nil
	}
	out := make([]string, len(r.profileNames))
	copy(out, r.profileNames)
	return out
}

// Fallback returns the configured fallback profile name.
func (r *Router) Fallback() string {
	if r == nil {
		return ""
	}
	return r.fallback
}

// renderDescriptions formats the profile catalog the classifier sees. Each
// line is "- name: description". If a description wasn't provided for a
// profile, we synthesise one from its skills/categories so the classifier
// has something to anchor on.
func renderDescriptions(
	names []string,
	profiles map[string]types.ProfileConfig,
	descriptions map[string]string,
) string {
	var b strings.Builder
	for _, name := range names {
		desc := descriptions[name]
		if desc == "" {
			desc = synthesiseDescription(profiles[name])
		}
		fmt.Fprintf(&b, "- %s: %s\n", name, desc)
	}
	return b.String()
}

// synthesiseDescription builds a fallback description from a profile's
// skills + categories when the user didn't provide one. The classifier
// can still route reasonably with these one-liners.
func synthesiseDescription(p types.ProfileConfig) string {
	parts := []string{}
	if len(p.Categories) > 0 {
		parts = append(parts, "category: "+strings.Join(p.Categories, ", "))
	}
	if len(p.Skills) > 0 {
		parts = append(parts, "skills: "+strings.Join(p.Skills, ", "))
	}
	if len(parts) == 0 {
		return "general-purpose"
	}
	return strings.Join(parts, "; ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
