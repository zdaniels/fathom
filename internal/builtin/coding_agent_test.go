package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodingAgentRegistry(t *testing.T) {
	if _, ok := CodingAgentSpecByName("claude"); !ok {
		t.Error("claude provider should be registered")
	}
	if _, ok := CodingAgentSpecByName("codex"); !ok {
		t.Error("codex provider should be registered")
	}
	if s, ok := CodingAgentSpecByName(""); !ok || s.Name != "Claude Code" {
		t.Errorf("empty provider should default to claude, got %q ok=%v", s.Name, ok)
	}
	if _, ok := CodingAgentSpecByName("nope"); ok {
		t.Error("unknown provider should not resolve")
	}
	if len(CodingAgentProviders()) < 2 {
		t.Error("expected at least claude + codex providers")
	}
}

func TestClaudeSpecArgs(t *testing.T) {
	spec := ClaudeCodeSpec()
	if !spec.PromptViaStdin {
		t.Error("claude sends the prompt on stdin")
	}
	args := strings.Join(spec.BuildArgs(CodingAgentOptions{Model: "haiku", ResumeSessionID: "sess-1"}), " ")
	for _, want := range []string{"-p", "--output-format json", "--permission-mode acceptEdits", "--model haiku", "--resume sess-1"} {
		if !strings.Contains(args, want) {
			t.Errorf("claude args missing %q: %s", want, args)
		}
	}
	// No resume flag when there's no session.
	if strings.Contains(strings.Join(spec.BuildArgs(CodingAgentOptions{}), " "), "--resume") {
		t.Error("no --resume should be emitted without a session id")
	}
}

func TestCodexSpecArgs(t *testing.T) {
	spec := CodexSpec()
	if spec.PromptViaStdin || !spec.UsesOutputFile {
		t.Error("codex takes the prompt as an arg and uses an output file")
	}
	if len(spec.StripEnvPrefixes) == 0 || spec.StripEnvPrefixes[0] != "OPENAI_API_KEY=" {
		t.Errorf("codex should strip OPENAI_API_KEY to use the subscription login, got %v", spec.StripEnvPrefixes)
	}
	args := strings.Join(spec.BuildArgs(CodingAgentOptions{Model: "gpt-5-codex", outFile: "/tmp/out"}), " ")
	for _, want := range []string{"exec", "--skip-git-repo-check", "--sandbox workspace-write", "--output-last-message /tmp/out", "--model gpt-5-codex"} {
		if !strings.Contains(args, want) {
			t.Errorf("codex args missing %q: %s", want, args)
		}
	}
	if strings.Contains(args, "--full-auto") {
		t.Error("--full-auto is not a valid codex exec flag")
	}
}

func TestCodexParseReadsOutputFile(t *testing.T) {
	spec := CodexSpec()
	f := filepath.Join(t.TempDir(), "out")
	_ = os.WriteFile(f, []byte("  the final answer  \n"), 0o644)
	run, err := spec.Parse(CodingAgentOptions{outFile: f}, []byte("noisy session transcript"), nil, nil)
	if err != nil || run.Result != "the final answer" {
		t.Errorf("codex parse = %+v, %v; want the output-file content", run, err)
	}
	// No output file + error → surfaces the failure.
	if _, err := spec.Parse(CodingAgentOptions{outFile: "/no/such"}, nil, []byte("401 unauthorized"), errTest); err == nil {
		t.Error("codex with no output + run error should error")
	}
}

var errTest = fmtErr("boom")

func fmtErr(s string) error { return &simpleErr{s} }

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }

func TestParseClaudeResult(t *testing.T) {
	ok := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s1","num_turns":2,"total_cost_usd":0.01}`)
	run, err := parseClaudeResult(CodingAgentOptions{}, ok, nil, nil)
	if err != nil || run.Result != "done" || run.SessionID != "s1" || run.NumTurns != 2 {
		t.Errorf("good parse = %+v, %v", run, err)
	}
	// is_error → error
	if _, err := parseClaudeResult(CodingAgentOptions{}, []byte(`{"subtype":"error_max_turns","is_error":true,"result":"hit the cap"}`), nil, nil); err == nil {
		t.Error("is_error result should surface as an error")
	}
	// non-JSON → error (with stderr detail)
	if _, err := parseClaudeResult(CodingAgentOptions{}, []byte("not json"), []byte("boom"), nil); err == nil {
		t.Error("unparseable output should error")
	}
}

func TestStripEnv(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "secret")
	t.Setenv("KEEP_ME", "yes")
	out := strings.Join(stripEnv([]string{"OPENAI_API_KEY="}), "\n")
	if strings.Contains(out, "OPENAI_API_KEY=") {
		t.Error("OPENAI_API_KEY should be stripped")
	}
	if !strings.Contains(out, "KEEP_ME=yes") {
		t.Error("unrelated env should be kept")
	}
}
