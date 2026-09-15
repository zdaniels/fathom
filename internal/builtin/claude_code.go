package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zdaniels/fathom/internal/agent"
	"github.com/zdaniels/fathom/internal/security"
)

// ClaudeCodeTool hands a coding task to the locally-installed Claude Code CLI
// running in headless mode, then returns its result to the Fathom agent.
//
// Auth: it deliberately runs under the user's existing Claude Code login (the
// subscription session from `claude login`) — NOT an API key. To make that
// happen we STRIP ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN from the child's
// environment; if either is present Claude Code switches to pay-per-token API
// billing instead of the subscription. With them gone it falls back to the
// stored OAuth credentials.
//
// Safety: default permission mode is `acceptEdits` — Claude Code may read and
// write files in the workspace without prompts, but cannot run arbitrary shell
// commands (those need an approval that doesn't exist in headless mode), so the
// blast radius is workspace file edits. It's gated by config (off by default)
// and policy (Action "claude-code"), bounded by --max-turns and a timeout.
func ClaudeCodeTool(defaultModel string, maxTurns int) agent.ToolDefinition {
	if maxTurns <= 0 {
		maxTurns = 30
	}
	return agent.ToolDefinition{
		Name: "claude_code",
		Description: "Hand a coding task to the local Claude Code CLI (running on your Claude subscription). " +
			"Use for real implementation work — writing/editing files, building features, fixing bugs — that benefits " +
			"from a dedicated coding agent. It works in the workspace and returns a summary of what it did. " +
			"Pick the model with `model` (haiku for speed/cost, sonnet for balance, opus for hard problems).",
		SkillName: "claude-code",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"task": map[string]interface{}{
					"type":        "string",
					"description": "The complete coding task for Claude Code, with enough context to act without follow-up questions.",
				},
				"model": map[string]interface{}{
					"type":        "string",
					"description": "Claude model to use: \"haiku\", \"sonnet\", \"opus\", or a full model id. Omit for the configured default.",
				},
				"dir": map[string]interface{}{
					"type":        "string",
					"description": "Where to run. An ABSOLUTE path runs Claude Code directly in that project (e.g. \"/Users/me/dev/myapp\") — use this to work in the user's live codebase. A relative path is a subdirectory of the agent workspace. Omit to use the workspace root.",
				},
			},
			"required": []string{"task"},
		},
		Execute: func(ctx context.Context, p map[string]interface{}, tctx agent.ToolContext) (interface{}, error) {
			task := strings.TrimSpace(asStr(p["task"]))
			if task == "" {
				return nil, fmt.Errorf("claude_code: 'task' is required")
			}
			// Policy gate (auditable, rule-able). Default policy allows an
			// unrecognised action, so the config flag is the primary gate.
			if tctx.Policy != nil {
				if dec := tctx.Policy.Evaluate(security.PolicyContext{
					Action: "claude-code", Command: trunc(task, 200),
				}); dec.Decision == "deny" {
					return nil, fmt.Errorf("claude_code blocked by policy: %s", dec.Reason)
				}
			}

			workDir, derr := resolveClaudeCodeDir(asStr(p["dir"]))
			if derr != nil {
				return nil, derr
			}

			model := strings.TrimSpace(asStr(p["model"]))
			if model == "" {
				model = defaultModel
			}

			run, err := RunCodingAgent(ctx, ClaudeCodeSpec(), CodingAgentOptions{
				Prompt: task, Model: model, WorkDir: workDir, MaxTurns: maxTurns,
			})
			if err != nil {
				return nil, err
			}
			return map[string]interface{}{
				"result":     run.Result,
				"model":      model,
				"session_id": run.SessionID,
				"num_turns":  run.NumTurns,
				"cost_usd":   run.CostUSD,
			}, nil
		},
	}
}

// resolveClaudeCodeDir picks the directory Claude Code runs in.
//
//   - empty  → the agent workspace (so by default claude_code operates in the
//     same place as the rest of the agent's file tools).
//   - absolute → that exact directory (the user's live codebase), required to
//     exist. This intentionally escapes the workspace: claude_code is an
//     opt-in, policy-gated tool whose whole purpose is working in real repos.
//   - relative → a subpath of the workspace, path-traversal guarded.
func resolveClaudeCodeDir(dir string) (string, error) {
	d := strings.TrimSpace(dir)
	if d == "" {
		return workspaceRoot(), nil
	}
	if filepath.IsAbs(d) {
		info, err := os.Stat(d)
		if err != nil || !info.IsDir() {
			return "", fmt.Errorf("claude_code: dir %q is not an existing directory", d)
		}
		return filepath.Clean(d), nil
	}
	return resolveInWorkspace(d)
}

func asStr(v interface{}) string {
	s, _ := v.(string)
	return s
}
