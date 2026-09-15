package builtin

// Reusable driver for external "coding agent" CLIs (Claude Code, OpenAI Codex,
// …) run headlessly under the user's interactive/subscription login. The
// claude_code tool and Claude/Codex takeover modes all go through here; adding
// a new provider is one CodingAgentSpec.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/zdaniels/fathom/internal/streamtext"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// CodingAgentOptions configures one headless run.
type CodingAgentOptions struct {
	stream          bool
	Prompt          string        // the task / message
	Model           string        // "" → the CLI's default for the account
	WorkDir         string        // directory to run in
	ResumeSessionID string        // "" → new session; else continue (if supported)
	MaxTurns        int           // 0 → 30
	Timeout         time.Duration // 0 → 5m

	// outFile is a per-run temp file the runner allocates when the spec sets
	// UsesOutputFile — the CLI writes its final message there (e.g. codex's
	// --output-last-message), and Parse reads it. Set by RunCodingAgent.
	outFile string
}

// CodingAgentRun is the normalized result across providers.
type CodingAgentRun struct {
	Result    string
	SessionID string  // "" when the provider doesn't expose one
	NumTurns  int     // 0 when unknown
	CostUSD   float64 // 0 when unknown
}

// CodingAgentSpec describes how to drive one external CLI agent. The generic
// runner handles exec/timeout/env; the spec supplies the per-provider bits.
type CodingAgentSpec struct {
	Name string // human label, e.g. "Claude Code"
	Bin  string // binary on PATH, e.g. "claude"
	// BuildArgs returns the argv after Bin for a headless run.
	BuildArgs func(o CodingAgentOptions) []string
	// PromptViaStdin sends the prompt on stdin (avoids arg-length/escaping
	// issues); when false the prompt is appended as the final argv element.
	PromptViaStdin bool
	// UsesOutputFile makes the runner allocate a temp file (opts.outFile) that
	// BuildArgs can point the CLI's "write final message here" flag at, and
	// that Parse reads back — cleaner than scraping a streaming transcript.
	UsesOutputFile bool
	// StripEnvPrefixes are env entries (matched by prefix, e.g. "OPENAI_API_KEY=")
	// removed from the child so the CLI uses its login rather than an API key.
	StripEnvPrefixes []string
	// Parse turns the run (opts + raw stdout/stderr + run error) into a result.
	Parse func(opts CodingAgentOptions, stdout, stderr []byte, runErr error) (*CodingAgentRun, error)
}

// codingAgents is the built-in provider registry. New CLIs slot in here.
var codingAgents = map[string]func() CodingAgentSpec{
	"claude": ClaudeCodeSpec,
	"codex":  CodexSpec,
}

// CodingAgentSpecByName returns the spec for a provider id ("" → "claude").
func CodingAgentSpecByName(name string) (CodingAgentSpec, bool) {
	if name == "" {
		name = "claude"
	}
	if f, ok := codingAgents[strings.ToLower(strings.TrimSpace(name))]; ok {
		return f(), true
	}
	return CodingAgentSpec{}, false
}

// CodingAgentProviders lists the registered provider ids (for the settings UI).
func CodingAgentProviders() []string {
	out := make([]string, 0, len(codingAgents))
	for k := range codingAgents {
		out = append(out, k)
	}
	return out
}

// RunCodingAgent runs spec headlessly with opts and returns the parsed result.
func RunCodingAgent(ctx context.Context, spec CodingAgentSpec, opts CodingAgentOptions) (*CodingAgentRun, error) {
	bin, err := exec.LookPath(spec.Bin)
	if err != nil {
		return nil, fmt.Errorf("%s CLI is not installed (no `%s` on PATH). Install it and sign in with your subscription first", spec.Name, spec.Bin)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	if spec.UsesOutputFile {
		f, ferr := os.CreateTemp("", "fathom-agent-out-*")
		if ferr != nil {
			return nil, ferr
		}
		opts.outFile = f.Name()
		_ = f.Close()
		defer os.Remove(opts.outFile)
	}
	opts.stream = spec.Name == "Claude Code" && streamtext.Enabled(ctx)
	args := spec.BuildArgs(opts)
	if !spec.PromptViaStdin {
		args = append(args, opts.Prompt)
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(cctx, bin, args...)
	c.Dir = opts.WorkDir
	if spec.PromptViaStdin {
		c.Stdin = strings.NewReader(opts.Prompt)
	}
	c.Env = stripEnv(spec.StripEnvPrefixes)
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	var live *claudeStreamWriter
	if opts.stream {
		live = &claudeStreamWriter{ctx: ctx}
		c.Stdout = live
	}
	c.WaitDelay = 2 * time.Second
	runErr := c.Run()
	if live != nil {
		if len(bytes.TrimSpace(live.pending)) > 0 {
			_, _ = live.Write([]byte("\n"))
		}
		stdout.Reset()
		stdout.Write(live.result)
	}
	if cctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("%s: timed out after %s", spec.Name, timeout)
	}
	return spec.Parse(opts, stdout.Bytes(), stderr.Bytes(), runErr)
}

// stripEnv returns the current environment minus any entries whose prefix
// matches, so the child CLI falls back to its subscription login.
func stripEnv(prefixes []string) []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, e := range src {
		drop := false
		for _, p := range prefixes {
			if strings.HasPrefix(e, p) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, e)
		}
	}
	return out
}

// ----- Claude Code provider (verified) -----

// ClaudeCodeSpec drives `claude -p --output-format json`. Auth: strips the
// Anthropic API-key vars so it uses the `claude login` subscription.
func ClaudeCodeSpec() CodingAgentSpec {
	return CodingAgentSpec{
		Name:             "Claude Code",
		Bin:              "claude",
		PromptViaStdin:   true,
		StripEnvPrefixes: []string{"ANTHROPIC_API_KEY=", "ANTHROPIC_AUTH_TOKEN="},
		BuildArgs: func(o CodingAgentOptions) []string {
			maxTurns := o.MaxTurns
			if maxTurns <= 0 {
				maxTurns = 30
			}
			args := []string{"-p", "--output-format", "json", "--permission-mode", "acceptEdits", "--max-turns", strconv.Itoa(maxTurns)}
			if o.stream {
				args[2] = "stream-json"
				args = append(args, "--verbose", "--include-partial-messages")
			}
			if o.Model != "" {
				args = append(args, "--model", o.Model)
			}
			if o.ResumeSessionID != "" {
				args = append(args, "--resume", o.ResumeSessionID)
			}
			return args
		},
		Parse: parseClaudeResult,
	}
}

func parseClaudeResult(_ CodingAgentOptions, stdout, stderr []byte, runErr error) (*CodingAgentRun, error) {
	if runErr != nil {
		return nil, fmt.Errorf("claude_code failed: %w", runErr)
	}
	var res struct {
		Subtype      string  `json:"subtype"`
		IsError      bool    `json:"is_error"`
		Result       string  `json:"result"`
		SessionID    string  `json:"session_id"`
		TotalCostUSD float64 `json:"total_cost_usd"`
		NumTurns     int     `json:"num_turns"`
	}
	if json.Unmarshal(bytes.TrimSpace(stdout), &res) != nil {
		detail := strings.TrimSpace(string(stderr))
		if detail == "" {
			detail = trunc(string(stdout), 2000)
		}
		if runErr != nil {
			return nil, fmt.Errorf("claude_code failed: %v\n%s", runErr, trunc(detail, 2000))
		}
		return nil, fmt.Errorf("claude_code: could not parse output: %s", trunc(detail, 2000))
	}
	if res.IsError || (res.Subtype != "" && res.Subtype != "success") {
		msg := res.Result
		if msg == "" {
			msg = strings.TrimSpace(string(stderr))
		}
		return nil, fmt.Errorf("claude_code did not complete (%s): %s", res.Subtype, trunc(msg, 2000))
	}
	return &CodingAgentRun{Result: res.Result, SessionID: res.SessionID, NumTurns: res.NumTurns, CostUSD: res.TotalCostUSD}, nil
}

// ----- OpenAI Codex provider (best-effort) -----

// CodexSpec drives the OpenAI Codex CLI non-interactively via `codex exec`,
// verified against codex-cli 0.135.0:
//   - `exec [PROMPT]` is non-interactive; prompt as the trailing arg.
//   - `--sandbox workspace-write` lets it edit files without prompts.
//   - `--skip-git-repo-check` so it runs in a non-git workspace.
//   - `--output-last-message <file>` writes ONLY the final agent message —
//     far cleaner than scraping the JSONL event stream off stdout.
//   - strips OPENAI_API_KEY so it uses the `codex login` (ChatGPT) session.
//
// JSONL thread.started events provide the explicit session ID for durable resume.
func CodexSpec() CodingAgentSpec {
	return CodingAgentSpec{
		Name:             "OpenAI Codex",
		Bin:              "codex",
		PromptViaStdin:   false, // codex exec takes the prompt as a trailing arg
		UsesOutputFile:   true,
		StripEnvPrefixes: []string{"OPENAI_API_KEY="},
		BuildArgs: func(o CodingAgentOptions) []string {
			args := []string{"exec", "--sandbox", "workspace-write"}
			if o.ResumeSessionID != "" {
				args = append(args, "resume")
			}
			args = append(args, "--json", "--skip-git-repo-check", "--output-last-message", o.outFile)
			if o.Model != "" {
				args = append(args, "--model", o.Model)
			}
			if o.ResumeSessionID != "" {
				args = append(args, o.ResumeSessionID)
			}
			return args
		},
		Parse: func(o CodingAgentOptions, stdout, stderr []byte, runErr error) (*CodingAgentRun, error) {
			if runErr != nil {
				return nil, fmt.Errorf("codex failed: %w", runErr)
			}
			sessionID := o.ResumeSessionID
			for _, line := range bytes.Split(stdout, []byte("\n")) {
				var e struct {
					Type     string
					ThreadID string `json:"thread_id"`
				}
				if json.Unmarshal(line, &e) == nil && e.Type == "thread.started" && e.ThreadID != "" {
					sessionID = e.ThreadID
				}
			}
			// The final message lands in the output file.
			final := ""
			if o.outFile != "" {
				if b, err := os.ReadFile(o.outFile); err == nil {
					final = strings.TrimSpace(string(b))
				}
			}
			if final == "" {
				final = strings.TrimSpace(string(stdout))
			}
			if final == "" {
				detail := strings.TrimSpace(string(stderr))
				if runErr != nil {
					return nil, fmt.Errorf("codex failed: %v\n%s", runErr, trunc(detail, 2000))
				}
				return nil, fmt.Errorf("codex produced no output: %s", trunc(detail, 2000))
			}
			return &CodingAgentRun{Result: final, SessionID: sessionID}, nil
		},
	}
}
