package builtin

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/zdaniels/fathom/internal/agent"
	"github.com/zdaniels/fathom/internal/security"
)

// PythonExecTool runs Python code in a Docker sandbox. Distinct from `bash`
// in three important ways:
//
//  1. NO network — container is launched with --network=none so nothing can
//     phone out, exfiltrate, or be tricked into curl-ing internal URLs.
//  2. NO host filesystem access — only /work is mounted (a per-call tempdir
//     destroyed afterward), so the script can't read your ~/.ssh.
//  3. Memory + CPU caps — 512MB / 1 CPU. Stops a runaway script burning
//     the laptop.
//
// Trades flexibility for safety. If you want unsandboxed Python, use
// `bash` to invoke python directly — Fathom won't stop you, but the policy
// can.
func PythonExecTool() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "python_exec",
		Description: `Run Python 3 code in an isolated Docker sandbox. No network, no host filesystem. Returns stdout, stderr, exit code.
Use for: data analysis (pandas, numpy), JSON wrangling, math, anything that benefits from a real Python REPL rather than shelling out.
Requires Docker installed. Hard limits: 60s timeout, 512MB RAM, no network, no filesystem outside /work (a tempdir that's destroyed after).
For unsandboxed shell access use bash instead — but bash is policy-gated.`,
		SkillName: "python",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"code": map[string]interface{}{
					"type":        "string",
					"description": "Python source to run. Stdout/stderr are captured. The script runs in /work which is empty + writable; anything you create there is included in the response only if you read it back to stdout.",
				},
				"timeoutSec": map[string]interface{}{
					"type":        "number",
					"description": "Per-call timeout in seconds (default 60, max 180).",
				},
				"image": map[string]interface{}{
					"type":        "string",
					"description": "Docker image to run (default python:3.12-slim). Pre-pull custom images via `docker pull` first.",
				},
			},
			"required": []string{"code"},
		},
		Execute: func(ctx context.Context, p map[string]interface{}, tctx agent.ToolContext) (interface{}, error) {
			code, _ := p["code"].(string)
			if strings.TrimSpace(code) == "" {
				return nil, fmt.Errorf("python_exec: code is required")
			}
			timeout := 60 * time.Second
			if v, ok := p["timeoutSec"].(float64); ok && v > 0 {
				if v > 180 {
					v = 180
				}
				timeout = time.Duration(v) * time.Second
			}
			image := stringOr(p, "image", "python:3.12-slim")

			// Network access goes through the same policy check as bash —
			// even though the container has --network=none, the user's
			// policy file might still want python_exec gated. We treat it
			// as a shell-exec call.
			if tctx.Policy != nil {
				if dec := tctx.Policy.Evaluate(security.PolicyContext{
					Action: "shell-exec", Command: "python:" + strings.SplitN(code, "\n", 2)[0],
				}); dec.Decision == "deny" {
					return nil, fmt.Errorf("python_exec blocked by policy: %s", dec.Reason)
				}
			}

			// Per-call tempdir mounted at /work inside the container.
			workDir, err := os.MkdirTemp("", "fathom-py-")
			if err != nil {
				return nil, err
			}
			defer os.RemoveAll(workDir)
			scriptPath := filepath.Join(workDir, "script.py")
			if err := os.WriteFile(scriptPath, []byte(code), 0o644); err != nil {
				return nil, err
			}

			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			cmd := exec.CommandContext(cctx, "docker", "run",
				"--rm",
				"--network=none",
				"--memory=512m",
				"--cpus=1.0",
				"--read-only",
				"--tmpfs=/tmp:rw,size=64m",
				"-v", workDir+":/work",
				"-w", "/work",
				image,
				"python", "/work/script.py",
			)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err = cmd.Run()
			exitCode := 0
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else if cctx.Err() == context.DeadlineExceeded {
				return nil, fmt.Errorf("python_exec: timed out after %s", timeout)
			} else if err != nil {
				// Distinguish "docker not installed" from real runtime errors.
				if strings.Contains(err.Error(), "executable file not found") || strings.Contains(err.Error(), "command not found") {
					return nil, fmt.Errorf("python_exec: Docker not installed — install Docker Desktop or use bash + system Python")
				}
				return nil, fmt.Errorf("python_exec: %w", err)
			}
			return map[string]interface{}{
				"stdout":   trunc(stdout.String(), 256*1024),
				"stderr":   trunc(stderr.String(), 32*1024),
				"exitCode": exitCode,
			}, nil
		},
	}
}
