package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zdaniels/fathom/internal/agent"
	"github.com/zdaniels/fathom/internal/security"
	"github.com/zdaniels/fathom/pkg/types"
)

func allowAllPolicy() *security.PolicyEngine {
	return security.NewPolicyEngine(types.PolicyConfig{
		Defaults: types.PermissionSet{
			Network: "allow", Filesystem: "read-write", Shell: "allow", Secrets: "accessible",
		},
	})
}

func denyShellPolicy() *security.PolicyEngine {
	return security.NewPolicyEngine(types.PolicyConfig{
		Defaults: types.PermissionSet{
			Network: "allow", Filesystem: "read-write", Shell: "deny", Secrets: "accessible",
		},
	})
}

func TestShellRunsCommand(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FANTAZM_WORKSPACE_ROOT", root)
	out, err := ShellTool().Execute(context.Background(),
		map[string]interface{}{"command": "echo hello && exit 0"},
		agent.ToolContext{Policy: allowAllPolicy()})
	if err != nil {
		t.Fatalf("shell: %v", err)
	}
	m := out.(map[string]interface{})
	if !strings.Contains(m["stdout"].(string), "hello") {
		t.Errorf("stdout = %q, want 'hello'", m["stdout"])
	}
	if m["exitCode"].(int) != 0 {
		t.Errorf("exitCode = %v, want 0", m["exitCode"])
	}
}

func TestShellCapturesNonZeroExit(t *testing.T) {
	t.Setenv("FANTAZM_WORKSPACE_ROOT", t.TempDir())
	out, err := ShellTool().Execute(context.Background(),
		map[string]interface{}{"command": "exit 42"},
		agent.ToolContext{Policy: allowAllPolicy()})
	if err != nil {
		t.Fatalf("shell: %v", err)
	}
	if out.(map[string]interface{})["exitCode"].(int) != 42 {
		t.Errorf("exitCode = %v, want 42", out)
	}
}

func TestShellBlockedByPolicy(t *testing.T) {
	t.Setenv("FANTAZM_WORKSPACE_ROOT", t.TempDir())
	_, err := ShellTool().Execute(context.Background(),
		map[string]interface{}{"command": "echo hi"},
		agent.ToolContext{Policy: denyShellPolicy()})
	if err == nil || !strings.Contains(err.Error(), "blocked by policy") {
		t.Errorf("shell under deny policy should be blocked, got: %v", err)
	}
}

func TestEditFileReplacesUniqueString(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FANTAZM_WORKSPACE_ROOT", root)
	path := filepath.Join(root, "code.go")
	_ = os.WriteFile(path, []byte("var x = 1\nvar y = 2\n"), 0o644)
	_, err := EditFileTool().Execute(context.Background(),
		map[string]interface{}{"path": "code.go", "old_string": "var x = 1", "new_string": "var x = 99"},
		agent.ToolContext{Policy: allowAllPolicy()})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "var x = 99") {
		t.Errorf("edit didn't land: %q", data)
	}
}

func TestEditFileRefusesAmbiguousMatch(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FANTAZM_WORKSPACE_ROOT", root)
	path := filepath.Join(root, "code.go")
	_ = os.WriteFile(path, []byte("foo\nfoo\n"), 0o644)
	_, err := EditFileTool().Execute(context.Background(),
		map[string]interface{}{"path": "code.go", "old_string": "foo", "new_string": "bar"},
		agent.ToolContext{Policy: allowAllPolicy()})
	if err == nil || !strings.Contains(err.Error(), "occurs 2 times") {
		t.Errorf("ambiguous match should error, got: %v", err)
	}
}

func TestEditFileReplaceAll(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FANTAZM_WORKSPACE_ROOT", root)
	path := filepath.Join(root, "code.go")
	_ = os.WriteFile(path, []byte("foo\nfoo\n"), 0o644)
	_, err := EditFileTool().Execute(context.Background(),
		map[string]interface{}{"path": "code.go", "old_string": "foo", "new_string": "bar", "replaceAll": true},
		agent.ToolContext{Policy: allowAllPolicy()})
	if err != nil {
		t.Fatalf("replaceAll: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "bar\nbar\n" {
		t.Errorf("replaceAll didn't work: %q", data)
	}
}

func TestEditFileRefusesMissingString(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FANTAZM_WORKSPACE_ROOT", root)
	path := filepath.Join(root, "code.go")
	_ = os.WriteFile(path, []byte("hello\n"), 0o644)
	_, err := EditFileTool().Execute(context.Background(),
		map[string]interface{}{"path": "code.go", "old_string": "nothere", "new_string": "x"},
		agent.ToolContext{Policy: allowAllPolicy()})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing string should error, got: %v", err)
	}
}

func TestGrepFindsPatternAcrossFiles(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FANTAZM_WORKSPACE_ROOT", root)
	_ = os.WriteFile(filepath.Join(root, "a.go"), []byte("var token = \"abc\"\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "b.go"), []byte("// nothing here\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "c.go"), []byte("if token == nil {\n"), 0o644)
	out, err := GrepTool().Execute(context.Background(),
		map[string]interface{}{"pattern": "token"},
		agent.ToolContext{Policy: allowAllPolicy()})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	count := out.(map[string]interface{})["count"].(int)
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
}

func TestGrepSkipsBinaryFiles(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FANTAZM_WORKSPACE_ROOT", root)
	// File with NUL byte in first 512 bytes — binary.
	_ = os.WriteFile(filepath.Join(root, "blob.bin"), []byte("FOO\x00BAR token\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "code.go"), []byte("token here\n"), 0o644)
	out, _ := GrepTool().Execute(context.Background(),
		map[string]interface{}{"pattern": "token"},
		agent.ToolContext{Policy: allowAllPolicy()})
	matches := out.(map[string]interface{})["matches"].([]map[string]interface{})
	for _, h := range matches {
		if strings.HasSuffix(h["file"].(string), ".bin") {
			t.Errorf("grep should skip binary files, hit: %v", h)
		}
	}
}

func TestGlobMatchesRecursive(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FANTAZM_WORKSPACE_ROOT", root)
	_ = os.MkdirAll(filepath.Join(root, "pkg", "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "main.go"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "pkg", "a.go"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "pkg", "sub", "b.go"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "README.md"), []byte("x"), 0o644)

	out, err := GlobTool().Execute(context.Background(),
		map[string]interface{}{"pattern": "**/*.go"},
		agent.ToolContext{Policy: allowAllPolicy()})
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	count := out.(map[string]interface{})["count"].(int)
	if count != 3 {
		t.Errorf("**/*.go count = %d, want 3", count)
	}
}

func TestGlobSinglePathPattern(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FANTAZM_WORKSPACE_ROOT", root)
	_ = os.WriteFile(filepath.Join(root, "README.md"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "CONTRIBUTING.md"), []byte("x"), 0o644)
	out, _ := GlobTool().Execute(context.Background(),
		map[string]interface{}{"pattern": "*.md"},
		agent.ToolContext{Policy: allowAllPolicy()})
	count := out.(map[string]interface{})["count"].(int)
	if count != 2 {
		t.Errorf("*.md count = %d, want 2", count)
	}
}

func TestGlobSkipsNodeModules(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FANTAZM_WORKSPACE_ROOT", root)
	_ = os.MkdirAll(filepath.Join(root, "node_modules", "react"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "node_modules", "react", "index.js"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "main.js"), []byte("x"), 0o644)
	out, _ := GlobTool().Execute(context.Background(),
		map[string]interface{}{"pattern": "**/*.js"},
		agent.ToolContext{Policy: allowAllPolicy()})
	files := out.(map[string]interface{})["files"].([]string)
	for _, f := range files {
		if strings.HasPrefix(f, "node_modules") {
			t.Errorf("glob should skip node_modules, hit: %v", f)
		}
	}
}
