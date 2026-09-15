package builtin

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/zdaniels/fathom/internal/agent"
	"github.com/zdaniels/fathom/internal/security"
)

// SystemRunTool exposes AppleScript execution to the agent. macOS-only —
// returns a clear "not supported" error on Linux/Windows so the model can
// react rather than getting a cryptic shell error.
//
// Why AppleScript instead of just bash: most "drive other apps on this Mac"
// flows speak AppleScript (Messages, Mail, Calendar, Notes, Finder,
// system events). Wrapping it in its own tool means the agent picks the
// right primitive for "send my wife an iMessage" vs "list files in
// /tmp" — and lets policy gate them separately.
//
// Policy: treated as a shell-exec action so the user's `shell: deny`
// default catches it. If you want it more granular, add a rule that
// matches Action=="applescript".
func SystemRunTool() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "system_run",
		Description: `Run AppleScript on this macOS machine. Use to drive Apple apps the agent doesn't have a dedicated skill for — Notes, Mail, Calendar, Reminders, Finder, System Events, Music, Safari.

Returns stdout, stderr, exit code. Hard 30s timeout. macOS only — returns an error on Linux/Windows.

Examples:
  - 'tell application "Messages" to send "running late" to buddy "+15551234567"'
  - 'tell application "Reminders" to make new reminder with properties {name:"pick up groceries"}'
  - 'tell application "System Events" to keystroke "v" using {command down}'

Policy-gated as shell-exec — your fathom.policy.yaml's shell setting controls this.`,
		SkillName: "system",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"script": map[string]interface{}{
					"type":        "string",
					"description": "AppleScript source. Multi-line is fine; the script is passed via stdin so quote escaping is straightforward.",
				},
				"timeoutSec": map[string]interface{}{
					"type":        "number",
					"description": "Per-call timeout in seconds (default 30, max 120).",
				},
			},
			"required": []string{"script"},
		},
		Execute: func(ctx context.Context, p map[string]interface{}, tctx agent.ToolContext) (interface{}, error) {
			if runtime.GOOS != "darwin" {
				return nil, fmt.Errorf("system_run is macOS-only — this host runs %s", runtime.GOOS)
			}
			script, _ := p["script"].(string)
			if strings.TrimSpace(script) == "" {
				return nil, fmt.Errorf("system_run: script is required")
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
					Action:  "shell-exec",
					Command: "applescript:" + strings.SplitN(script, "\n", 2)[0],
				}); dec.Decision == "deny" {
					return nil, fmt.Errorf("system_run blocked by policy: %s", dec.Reason)
				}
			}

			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			cmd := exec.CommandContext(cctx, "osascript", "-")
			cmd.Stdin = strings.NewReader(script)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			exitCode := 0
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else if cctx.Err() == context.DeadlineExceeded {
				return nil, fmt.Errorf("system_run: timed out after %s", timeout)
			} else if err != nil && exitCode == 0 {
				return nil, fmt.Errorf("system_run: %w", err)
			}
			return map[string]interface{}{
				"stdout":   trunc(stdout.String(), 256*1024),
				"stderr":   trunc(stderr.String(), 32*1024),
				"exitCode": exitCode,
			}, nil
		},
	}
}
