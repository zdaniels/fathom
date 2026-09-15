package agentfactory

// Takeover mode: when cfg.Takeover is enabled, EVERY user message is routed
// straight to an external coding-agent CLI (Claude Code, OpenAI Codex, …)
// instead of the Fathom routed agent. The user is effectively chatting with
// that agent through Fathom's surfaces, with each thread kept as one session
// (when the provider supports resume).

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zdaniels/fathom/internal/builtin"
	"github.com/zdaniels/fathom/pkg/types"
)

// takeoverEnabled reports whether takeover mode is on.
func takeoverEnabled(cfg types.Config) bool {
	return cfg.Takeover != nil && cfg.Takeover.Enabled
}

// TakeoverDesc renders the external agent a takeover config routes to, e.g.
// "claude (provider default model)" when no model is pinned, or "claude:sonnet"
// when one is. Shared by the CLI + gateway banners and the model-question
// interceptor so every surface reports takeover identically.
func TakeoverDesc(tc *types.TakeoverConfig) string {
	provider := "claude"
	if tc != nil && tc.Provider != "" {
		provider = tc.Provider
	}
	if tc != nil && tc.Model != "" {
		return provider + ":" + tc.Model
	}
	return provider + " (provider default model)"
}

// BuildTakeoverHandler returns a gateway MessageHandler that sends each message
// to the configured provider and returns its reply, keeping a per-thread
// session for continuity. Returns an error for an unknown provider.
func BuildTakeoverHandler(cfg types.Config) (func(ctx context.Context, msg types.ChannelMessage, sess types.Session) (string, error), error) {
	provider, model := "claude", ""
	if cfg.Takeover != nil {
		if cfg.Takeover.Provider != "" {
			provider = cfg.Takeover.Provider
		}
		model = cfg.Takeover.Model
	}
	spec, ok := builtin.CodingAgentSpecByName(provider)
	if !ok {
		return nil, fmt.Errorf("unknown takeover provider %q (known: %s)", provider, strings.Join(builtin.CodingAgentProviders(), ", "))
	}

	var mu sync.Mutex
	sessions := map[string]string{} // threadID → provider session id

	return func(ctx context.Context, msg types.ChannelMessage, sess types.Session) (string, error) {
		thread := msg.ChannelID
		mu.Lock()
		resume := sessions[thread]
		mu.Unlock()

		run, err := builtin.RunCodingAgent(ctx, spec, builtin.CodingAgentOptions{
			Prompt:          msg.Text,
			Model:           model,
			WorkDir:         builtin.WorkspaceRoot(),
			ResumeSessionID: resume,
			Timeout:         10 * time.Minute, // chat turns can be long coding runs
		})
		if err != nil {
			return "", err
		}
		if run.SessionID != "" {
			mu.Lock()
			sessions[thread] = run.SessionID
			mu.Unlock()
		}
		return run.Result, nil
	}, nil
}
