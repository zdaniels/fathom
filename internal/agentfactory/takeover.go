package agentfactory

// Takeover mode: when cfg.Takeover is enabled, EVERY user message is routed
// straight to an external coding-agent CLI (Claude Code, OpenAI Codex, …)
// instead of the Fathom routed agent. The user is effectively chatting with
// that agent through Fathom's surfaces, with each thread kept as one session
// (when the provider supports resume).

import (
	"context"
	"fmt"
	"os/exec"
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

	if _, err := exec.LookPath(spec.Bin); err != nil {
		return nil, fmt.Errorf("takeover CLI unavailable: %w", err)
	}
	var mu sync.Mutex
	type conversationKey struct{ user, channel, thread string }
	type conversation struct {
		gate   chan struct{}
		resume string
	}
	sessions := map[conversationKey]*conversation{}

	return func(ctx context.Context, msg types.ChannelMessage, sess types.Session) (string, error) {
		// REST and scheduled invocations are independent, one-shot requests.
		state := &conversation{gate: make(chan struct{}, 1)}
		persistentPath := ""
		if msg.ChannelID != "" && msg.ChannelType != "rest" && msg.ChannelType != "scheduler" {
			key := conversationKey{sess.UserID, msg.ChannelType, msg.ChannelID}
			persistentPath = resumePath(cfg.DataDir, provider, model, builtin.WorkspaceRoot(), sess.UserID, msg.ChannelType, msg.ChannelID)
			mu.Lock()
			if existing := sessions[key]; existing != nil {
				state = existing
			} else {
				sessions[key] = state
			}
			mu.Unlock()
		}
		// Serialize the whole turn so concurrent follow-ups see the latest session.
		select {
		case state.gate <- struct{}{}:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		defer func() { <-state.gate }()
		if persistentPath != "" {
			resume, err := loadResume(persistentPath)
			if err != nil {
				return "", err
			}
			state.resume = resume
		}

		run, err := builtin.RunCodingAgent(ctx, spec, builtin.CodingAgentOptions{
			Prompt:          msg.Text,
			Model:           model,
			WorkDir:         builtin.WorkspaceRoot(),
			ResumeSessionID: state.resume,
			Timeout:         10 * time.Minute, // chat turns can be long coding runs
		})
		if err != nil {
			return "", err
		}
		if run.SessionID != "" {
			state.resume = run.SessionID
			if err := saveResume(persistentPath, run.SessionID); err != nil {
				return "", fmt.Errorf("save conversation continuity: %w", err)
			}
		}
		return run.Result, nil
	}, nil
}

// createTakeover selects the active backend once for CLI, HTTP, and scheduler.
func createTakeover(cfg types.Config, opts Options) (*Result, error) {
	h, err := BuildTakeoverHandler(cfg)
	if err != nil {
		return nil, err
	}
	vault, mesh, err := openSecurity(cfg, opts)
	if err != nil {
		return nil, err
	}
	return &Result{Ready: true, Description: "takeover → " + TakeoverDesc(cfg.Takeover), Vault: vault, Security: mesh,
		Handler: func(ctx context.Context, msg types.ChannelMessage, sess types.Session) (string, error) {
			if !mesh.Policy.UserAllowed(sess.UserID) {
				return "", fmt.Errorf("execution role required")
			}
			mesh.Audit.Log(sess.ID, sess.UserID, types.AuditLLMRequest, map[string]interface{}{"backend": "takeover", "channel": msg.ChannelType}, types.PolicyAllow)
			if err := mesh.Audit.Err(); err != nil {
				return "", fmt.Errorf("audit unavailable: %w", err)
			}
			return h(ctx, msg, sess)
		}}, nil
}
