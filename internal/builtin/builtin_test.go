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

func TestFileEditorRejectsSiblingPathTraversal(t *testing.T) {
	// Regression for the TS PR #1 review finding: bare startsWith(root)
	// let "../work-secrets/x" escape a workspace at "/Users/alice/work"
	// because "/Users/alice/work-secrets/x" starts with "/Users/alice/work".
	// We fix this with the separator-boundary check.
	root := t.TempDir()
	sibling := root + "-sibling"
	os.MkdirAll(sibling, 0o755)
	defer os.RemoveAll(sibling)
	os.WriteFile(filepath.Join(sibling, "secret.txt"), []byte("leak!"), 0o644)

	t.Setenv("FANTAZM_WORKSPACE_ROOT", root)
	tools := FileEditorTools()
	var read agent.ToolDefinition
	for _, tl := range tools {
		if tl.Name == "read_file" {
			read = tl
			break
		}
	}
	if read.Execute == nil {
		t.Fatal("read_file tool missing")
	}

	_, err := read.Execute(context.Background(),
		map[string]interface{}{"path": "../" + filepath.Base(sibling) + "/secret.txt"},
		agent.ToolContext{})
	if err == nil {
		t.Fatal("sibling-dir traversal should be rejected")
	}
	if !strings.Contains(err.Error(), "outside workspace") {
		t.Errorf("error should mention 'outside workspace', got %q", err.Error())
	}
}

func TestFileEditorReadsLegitRelativePath(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "hello.txt"), []byte("world"), 0o644)
	t.Setenv("FANTAZM_WORKSPACE_ROOT", root)

	read := findTool(FileEditorTools(), "read_file")
	out, err := read.Execute(context.Background(),
		map[string]interface{}{"path": "hello.txt"},
		agent.ToolContext{})
	if err != nil {
		t.Fatalf("legit read: %v", err)
	}
	if s, _ := out.(string); s != "world" {
		t.Errorf("read = %q, want %q", out, "world")
	}
}

func TestFileEditorWriteCreatesParentDirs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FANTAZM_WORKSPACE_ROOT", root)
	write := findTool(FileEditorTools(), "write_file")
	_, err := write.Execute(context.Background(),
		map[string]interface{}{"path": "sub/dir/note.md", "content": "hi"},
		agent.ToolContext{})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "sub/dir/note.md"))
	if err != nil || string(data) != "hi" {
		t.Errorf("file not written: data=%q err=%v", data, err)
	}
}

func TestNotesCreateSearchListRoundTrip(t *testing.T) {
	tools := NotesTools()
	create := findTool(tools, "create_note")
	search := findTool(tools, "search_notes")
	list := findTool(tools, "list_notes")

	res, err := create.Execute(context.Background(), map[string]interface{}{
		"title":   "Deploy plan",
		"content": "Roll out at 9am Monday",
		"tags":    []interface{}{"ops", "release"},
	}, agent.ToolContext{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id, _ := res.(map[string]string)["id"]
	if id == "" {
		t.Fatal("create returned no id")
	}

	// search should find it (case-insensitive).
	sres, _ := search.Execute(context.Background(),
		map[string]interface{}{"query": "deploy"},
		agent.ToolContext{})
	notes := sres.(map[string]interface{})["notes"].([]Note)
	if len(notes) == 0 || notes[0].ID != id {
		t.Errorf("search didn't find note: %v", notes)
	}

	// list filtered by tag.
	lres, _ := list.Execute(context.Background(),
		map[string]interface{}{"tag": "ops"},
		agent.ToolContext{})
	if len(lres.(map[string]interface{})["notes"].([]Note)) == 0 {
		t.Errorf("list-by-tag missed the note")
	}

	// list filtered by a tag that doesn't exist returns empty.
	lres2, _ := list.Execute(context.Background(),
		map[string]interface{}{"tag": "no-such-tag"},
		agent.ToolContext{})
	if len(lres2.(map[string]interface{})["notes"].([]Note)) != 0 {
		t.Errorf("list with unknown tag should be empty, got %v", lres2)
	}
}

func TestFileEditorWriteBlockedByReadOnlyPolicy(t *testing.T) {
	// The headline fix: write_file now actually consults the policy engine.
	// Under filesystem: read-only defaults, a write must be denied even when
	// the path resolves cleanly within the workspace.
	root := t.TempDir()
	t.Setenv("FANTAZM_WORKSPACE_ROOT", root)
	policy := security.NewPolicyEngine(types.PolicyConfig{
		Defaults: types.PermissionSet{Network: "allow", Filesystem: "read-only", Shell: "deny"},
	})

	write := findTool(FileEditorTools(), "write_file")
	_, err := write.Execute(context.Background(),
		map[string]interface{}{"path": "blocked.txt", "content": "should not land"},
		agent.ToolContext{Policy: policy})
	if err == nil {
		t.Fatal("write under read-only policy should be denied")
	}
	if !strings.Contains(err.Error(), "blocked by policy") {
		t.Errorf("error should mention policy block, got %q", err.Error())
	}
	if _, statErr := os.Stat(filepath.Join(root, "blocked.txt")); statErr == nil {
		t.Error("file should NOT exist on disk after policy denial")
	}
}

func TestFileEditorReadAllowedUnderReadOnlyPolicy(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FANTAZM_WORKSPACE_ROOT", root)
	_ = os.WriteFile(filepath.Join(root, "ok.txt"), []byte("readable"), 0o644)
	policy := security.NewPolicyEngine(types.PolicyConfig{
		Defaults: types.PermissionSet{Network: "allow", Filesystem: "read-only", Shell: "deny"},
	})
	read := findTool(FileEditorTools(), "read_file")
	out, err := read.Execute(context.Background(),
		map[string]interface{}{"path": "ok.txt"},
		agent.ToolContext{Policy: policy})
	if err != nil {
		t.Fatalf("read under read-only policy should be allowed: %v", err)
	}
	if s, _ := out.(string); s != "readable" {
		t.Errorf("read = %q, want 'readable'", out)
	}
}

func TestForSkillsFiltersCorrectly(t *testing.T) {
	tools := ForSkills([]string{"notes"})
	for _, tl := range tools {
		if tl.SkillName != "notes" {
			t.Errorf("ForSkills(['notes']) included tool from %q", tl.SkillName)
		}
	}
	if len(tools) == 0 {
		t.Error("ForSkills(['notes']) returned empty")
	}
}

func findTool(tools []agent.ToolDefinition, name string) agent.ToolDefinition {
	for _, t := range tools {
		if t.Name == name {
			return t
		}
	}
	return agent.ToolDefinition{}
}
