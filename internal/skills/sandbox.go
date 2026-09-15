package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/zdaniels/fathom/internal/brandenv"
	"os"
	"os/exec"
	"time"
)

// Sandbox runs a skill subprocess with a stripped environment. By
// default it dispatches Node (TypeScript) skills via runner.js. When
// the per-invocation `Language` is "python", it dispatches Python
// skills via runner.py with `python3` on PATH. Both runners speak the
// same stdin/stdout JSON protocol (see runner.js / runner.py).
type Sandbox struct {
	timeoutMs    int
	runnerPath   string // runner.js
	runnerPyPath string // runner.py (optional; empty disables python skills)
	nodeBin      string
	pythonBin    string
	isolation    sandboxConfig // OS-level deny of vault/key paths (see sandbox_isolate.go)
}

// SandboxInvocation is the shape the runner expects on stdin.
type SandboxInvocation struct {
	EntryPoint   string            `json:"entryPoint"`
	FunctionName string            `json:"functionName"`
	Input        interface{}       `json:"input"`
	Secrets      map[string]string `json:"secrets,omitempty"`

	// Language selects which runner to spawn. Empty / "node" → runner.js.
	// "python" → runner.py. Other values error out at Invoke time.
	Language string `json:"-"`

	// Egress is passed in-band so the runner can populate ctx.fetch only when
	// the host wants the skill to be able to make outbound calls.
	Egress *EgressContext `json:"-"`
}

// EgressContext is set in the subprocess env; not in the stdin JSON.
type EgressContext struct {
	URL   string
	Token string
}

// NewSandbox returns a Sandbox using the default Node binary on PATH.
// runnerPath is the runner.js extracted via EnsureRunnerOnDisk.
// Python skills require WithPythonRunner before they'll dispatch.
func NewSandbox(runnerPath string, timeoutMs int) *Sandbox {
	if timeoutMs <= 0 {
		timeoutMs = 10_000
	}
	return &Sandbox{
		timeoutMs:  timeoutMs,
		runnerPath: runnerPath,
		nodeBin:    "node",
		pythonBin:  "python3",
	}
}

// WithPythonRunner enables Python skill dispatch. Call after
// EnsurePythonRunnerOnDisk; before that point, manifests with
// language: python error at Invoke time with a clear message.
func (s *Sandbox) WithPythonRunner(runnerPyPath string) *Sandbox {
	s.runnerPyPath = runnerPyPath
	return s
}

// WithIsolation makes every skill subprocess run inside an OS sandbox that
// denies access to the given secret-bearing paths (typically the vault file
// and the master-key file). No-op on platforms without a sandbox tool — the
// runner still launches, just without the extra deny layer.
func (s *Sandbox) WithIsolation(protectPaths ...string) *Sandbox {
	s.isolation = newSandboxConfig(protectPaths...)
	return s
}

// Invoke runs the skill subprocess and parses its JSON reply. Dispatches
// to runner.js (Node) or runner.py (Python) based on inv.Language.
func (s *Sandbox) Invoke(ctx context.Context, inv SandboxInvocation) (interface{}, error) {
	cctx, cancel := context.WithTimeout(ctx, time.Duration(s.timeoutMs)*time.Millisecond)
	defer cancel()

	// Build the (binary, args, env) tuple for the runner the manifest
	// asked for. `language: ""` (legacy) and `language: node` use the
	// Node runner; `python` uses the Python runner; anything else is a
	// configuration error surfaced early.
	var bin string
	var binArgs []string
	env := []string{
		"PATH=" + envOr("PATH", "/usr/bin:/bin"),
		"HOME=" + envOr("HOME", "/tmp"),
		"TMPDIR=" + envOr("TMPDIR", "/tmp"),
	}
	switch inv.Language {
	case "", "node":
		// --experimental-strip-types lets Node 22.6+ load `index.ts`
		// directly: type annotations / interfaces / `as` casts are
		// stripped at parse time, no esbuild step needed. Node 23+ has
		// this on by default; the flag is a safe no-op there. Skills
		// MUST avoid enum / namespace / parameter properties — anything
		// that needs real transformation.
		//
		// We deliberately do NOT pass --frozen-intrinsics: it freezes
		// Error, Array, etc, which breaks npm deps that customise
		// Error.prepareStackTrace for nicer stack traces (Baileys, pino,
		// Sentry, and a long tail). The freeze was defense-in-depth —
		// the actual security boundary is subprocess isolation + egress
		// proxy + vault-scoped secrets + policy engine. A skill mutating
		// its own Error prototype only affects its own subprocess,
		// which exits after one invocation anyway.
		//
		// --disallow-code-generation-from-strings stays: blocks eval /
		// new Function() in skill code (the relevant attack surface).
		bin = s.nodeBin
		binArgs = []string{
			"--disallow-code-generation-from-strings",
			"--no-deprecation",
			"--experimental-strip-types",
			"--no-warnings",
			s.runnerPath,
		}
		env = append(env, "NODE_ENV="+envOr("NODE_ENV", "production"))
	case "python":
		if s.runnerPyPath == "" {
			return nil, fmt.Errorf("python runner not registered — call Sandbox.WithPythonRunner first")
		}
		bin = s.pythonBin
		binArgs = []string{s.runnerPyPath}
		// PYTHONDONTWRITEBYTECODE avoids polluting the skill dir with
		// __pycache__/ entries the host then has to clean up.
		env = append(env,
			"PYTHONDONTWRITEBYTECODE=1",
			"PYTHONUNBUFFERED=1",
		)
	default:
		return nil, fmt.Errorf("unknown skill language %q (want \"node\" or \"python\")", inv.Language)
	}

	if inv.Egress != nil {
		env = append(env, "FANTAZM_PROXY_URL="+inv.Egress.URL, "FANTAZM_PROXY_TOKEN="+inv.Egress.Token)
	}

	// Launch through the OS sandbox when isolation is configured. The runner
	// inherits the stripped env regardless of whether a sandbox wraps it.
	var cmd *exec.Cmd
	mode := brandenv.Get("FATHOM_SKILL_SANDBOX")
	switch mode {
	case "required":
		var err error
		cmd, err = s.isolatedCommand(cctx, &inv, binArgs)
		if err != nil {
			return nil, err
		}
		// Killing the Docker client does not stop its container. Explicitly
		// remove the named container on timeout, cancellation, or completion.
		name := cmd.Args[4]
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = exec.CommandContext(cleanup, cmd.Path, "rm", "--force", name).Run()
		}()
	case "", "1", "trusted":
		cmd = s.isolation.wrap(cctx, bin, binArgs)
		cmd.Env = env
	default:
		return nil, fmt.Errorf("unknown FATHOM_SKILL_SANDBOX mode %q", mode)
	}

	stdin, _ := json.Marshal(struct {
		EntryPoint   string            `json:"entryPoint"`
		FunctionName string            `json:"functionName"`
		Input        interface{}       `json:"input"`
		Secrets      map[string]string `json:"secrets,omitempty"`
	}{
		EntryPoint:   inv.EntryPoint,
		FunctionName: inv.FunctionName,
		Input:        inv.Input,
		Secrets:      inv.Secrets,
	})

	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if cctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("skill execution timed out after %dms", s.timeoutMs)
	}

	if err != nil {
		return nil, fmt.Errorf("skill process failed: %w: %s", err, truncate(stderr.String(), 500))
	}
	if stdout.Len() == 0 {
		if err != nil {
			return nil, fmt.Errorf("skill produced no output (%v): %s", err, truncate(stderr.String(), 500))
		}
		return nil, errors.New("skill produced no output")
	}

	var resp struct {
		OK     bool        `json:"ok"`
		Result interface{} `json:"result"`
		Error  string      `json:"error"`
	}
	if jerr := json.Unmarshal(stdout.Bytes(), &resp); jerr != nil {
		return nil, fmt.Errorf("skill produced invalid JSON output: %v\nstdout: %s",
			jerr, truncate(stdout.String(), 500))
	}
	if !resp.OK {
		return nil, errors.New(resp.Error)
	}
	return resp.Result, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...[truncated]"
}
