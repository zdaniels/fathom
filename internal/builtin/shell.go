package builtin

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/zdaniels/fathom/internal/agent"
	"github.com/zdaniels/fathom/internal/security"
)

// ShellTool runs a shell command in the workspace and returns stdout, stderr,
// and exit code. Policy-gated: defaults deny in personal mode unless the
// policy file allows shell-exec. Designed for code work — tests, linters,
// builds, git, package managers.
//
// Hard limits: 30s timeout, 256KiB output truncation, no interactive stdin.
func ShellTool() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "bash",
		Description: `Run a shell command in the workspace. Returns stdout, stderr, exit code.
Use for: running tests, linters, builds, git, package managers, file system queries (ls, find).
Hard limits: 30s timeout (configurable up to 120s), 256KiB output truncated, no stdin.
Policy-gated — denied by default in personal mode; the user must enable shell-exec in fathom.policy.yaml.`,
		SkillName: "shell",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"command": map[string]interface{}{
					"type":        "string",
					"description": "Command to run via `sh -c`. Quote arguments containing spaces.",
				},
				"timeoutSec": map[string]interface{}{
					"type":        "number",
					"description": "Per-call timeout in seconds (default 30, max 120).",
				},
			},
			"required": []string{"command"},
		},
		Execute: func(ctx context.Context, p map[string]interface{}, tctx agent.ToolContext) (interface{}, error) {
			cmd, _ := p["command"].(string)
			if strings.TrimSpace(cmd) == "" {
				return nil, fmt.Errorf("bash: empty command")
			}
			timeout := 30 * time.Second
			if v, ok := p["timeoutSec"].(float64); ok && v > 0 {
				if v > 120 {
					v = 120
				}
				timeout = time.Duration(v) * time.Second
			}
			if tctx.Policy != nil {
				if dec := tctx.Policy.Evaluate(security.PolicyContext{
					Action: "shell-exec", Command: cmd,
				}); dec.Decision == "deny" {
					return nil, fmt.Errorf("bash blocked by policy: %s", dec.Reason)
				}
			}

			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			c := exec.CommandContext(cctx, "sh", "-c", cmd)
			c.Dir = workspaceRoot()
			var stdout, stderr bytes.Buffer
			c.Stdout = &stdout
			c.Stderr = &stderr
			err := c.Run()
			exitCode := 0
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else if cctx.Err() == context.DeadlineExceeded {
				return nil, fmt.Errorf("bash: timed out after %s", timeout)
			} else if err != nil && exitCode == 0 {
				return nil, fmt.Errorf("bash: %w", err)
			}
			return map[string]interface{}{
				"stdout":   trunc(stdout.String(), 256*1024),
				"stderr":   trunc(stderr.String(), 32*1024),
				"exitCode": exitCode,
			}, nil
		},
	}
}

func trunc(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n…(truncated)"
}
