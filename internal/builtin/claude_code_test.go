package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveClaudeCodeDir(t *testing.T) {
	// Scope the workspace to a temp dir for the relative/empty cases.
	ws := t.TempDir()
	t.Setenv("FANTAZM_WORKSPACE_ROOT", ws)

	// Empty → workspace root.
	got, err := resolveClaudeCodeDir("")
	if err != nil || got != ws {
		t.Errorf("empty dir = %q, %v; want workspace %q", got, err, ws)
	}

	// Absolute existing dir → that exact dir (the user's live codebase), even
	// though it's OUTSIDE the workspace.
	project := t.TempDir()
	got, err = resolveClaudeCodeDir(project)
	if err != nil || got != filepath.Clean(project) {
		t.Errorf("absolute project dir = %q, %v; want %q", got, err, project)
	}

	// Absolute but non-existent → clear error, no silent fallback.
	if _, err := resolveClaudeCodeDir("/no/such/dir/here-xyz"); err == nil ||
		!strings.Contains(err.Error(), "not an existing directory") {
		t.Errorf("nonexistent abs dir: want error, got %v", err)
	}

	// Absolute path to a FILE (not a dir) → error.
	f := filepath.Join(project, "file.txt")
	_ = os.WriteFile(f, []byte("x"), 0o644)
	if _, err := resolveClaudeCodeDir(f); err == nil {
		t.Error("absolute path to a file should error")
	}

	// Relative → a subpath of the workspace.
	sub := filepath.Join(ws, "pkg")
	_ = os.MkdirAll(sub, 0o755)
	got, err = resolveClaudeCodeDir("pkg")
	if err != nil || got != sub {
		t.Errorf("relative dir = %q, %v; want %q", got, err, sub)
	}

	// Relative traversal outside the workspace → rejected by the guard.
	if _, err := resolveClaudeCodeDir("../escape"); err == nil {
		t.Error("relative ../escape should be rejected by the workspace guard")
	}
}
