package builtin

import (
	"context"
	"errors"
	"fmt"
	"github.com/zdaniels/fathom/internal/brandenv"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zdaniels/fathom/internal/agent"
)

// FileEditorTools returns read/write/list tools scoped to the workspace
// (FANTAZM_WORKSPACE_ROOT or cwd). The path-traversal guard uses the
// separator-boundary check — `startsWith(root)` alone would let
// `../work-secrets/x` escape when the workspace is `/Users/alice/work`.
func FileEditorTools() []agent.ToolDefinition {
	return []agent.ToolDefinition{
		{
			Name:        "read_file",
			Description: "Read a UTF-8 text file from the workspace",
			SkillName:   "file-editor",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}},
				"required":   []string{"path"},
			},
			Execute: func(ctx context.Context, p map[string]interface{}, tctx agent.ToolContext) (interface{}, error) {
				path, _ := p["path"].(string)
				abs, err := resolveInWorkspace(path)
				if err != nil {
					return nil, err
				}
				if tctx.Policy != nil {
					if dec := tctx.Policy.CheckFilesystem(abs, "read"); dec.Decision == "deny" {
						return nil, fmt.Errorf("read_file blocked by policy: %s", dec.Reason)
					}
				}
				data, err := os.ReadFile(abs)
				if err != nil {
					return nil, err
				}
				return string(data), nil
			},
		},
		{
			Name:        "write_file",
			Description: "Write UTF-8 text content to a file in the workspace (creates parent dirs)",
			SkillName:   "file-editor",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":    map[string]interface{}{"type": "string"},
					"content": map[string]interface{}{"type": "string"},
				},
				"required": []string{"path", "content"},
			},
			Execute: func(ctx context.Context, p map[string]interface{}, tctx agent.ToolContext) (interface{}, error) {
				path, _ := p["path"].(string)
				content, _ := p["content"].(string)
				abs, err := resolveInWorkspace(path)
				if err != nil {
					return nil, err
				}
				if tctx.Policy != nil {
					if dec := tctx.Policy.CheckFilesystem(abs, "write"); dec.Decision == "deny" {
						return nil, fmt.Errorf("write_file blocked by policy: %s", dec.Reason)
					}
				}
				if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
					return nil, err
				}
				if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
					return nil, err
				}
				rel, _ := filepath.Rel(workspaceRoot(), abs)
				return map[string]interface{}{"ok": true, "path": rel}, nil
			},
		},
		{
			Name:        "list_files",
			Description: "List files in a directory recursively, optionally filtered by a regex pattern",
			SkillName:   "file-editor",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"dir":     map[string]interface{}{"type": "string"},
					"pattern": map[string]interface{}{"type": "string"},
				},
				"required": []string{"dir"},
			},
			Execute: func(ctx context.Context, p map[string]interface{}, tctx agent.ToolContext) (interface{}, error) {
				dir, _ := p["dir"].(string)
				pattern, _ := p["pattern"].(string)
				absDir, err := resolveInWorkspace(dir)
				if err != nil {
					return nil, err
				}
				if tctx.Policy != nil {
					if dec := tctx.Policy.CheckFilesystem(absDir, "read"); dec.Decision == "deny" {
						return nil, fmt.Errorf("list_files blocked by policy: %s", dec.Reason)
					}
				}
				var re *regexp.Regexp
				if pattern != "" {
					re, err = regexp.Compile(pattern)
					if err != nil {
						return nil, fmt.Errorf("invalid pattern: %w", err)
					}
				}
				root := workspaceRoot()
				var out []string
				err = filepath.Walk(absDir, func(p string, info os.FileInfo, walkErr error) error {
					if walkErr != nil {
						return walkErr
					}
					name := info.Name()
					if info.IsDir() {
						if name == "node_modules" || strings.HasPrefix(name, ".") {
							return filepath.SkipDir
						}
						return nil
					}
					if re != nil && !re.MatchString(name) {
						return nil
					}
					rel, _ := filepath.Rel(root, p)
					out = append(out, rel)
					return nil
				})
				if err != nil {
					return nil, err
				}
				return map[string]interface{}{"files": out}, nil
			},
		},
	}
}

// WorkspaceRoot is the exported view of the agent workspace directory
// (FANTAZM_WORKSPACE_ROOT or the process cwd), for callers outside this
// package (e.g. Claude takeover mode).
func WorkspaceRoot() string { return workspaceRoot() }

func workspaceRoot() string {
	if v := brandenv.Get("FATHOM_WORKSPACE_ROOT"); v != "" {
		abs, _ := filepath.Abs(v)
		return abs
	}
	d, _ := os.Getwd()
	return d
}

// resolveInWorkspace is the SAFE join: must equal root exactly or be a true
// subpath (root + path separator). `startsWith(root)` alone is the classic
// path-traversal bug (sibling dirs sharing the name prefix slip through).
func resolveInWorkspace(input string) (string, error) {
	root := workspaceRoot()
	abs := filepath.Join(root, input)
	abs = filepath.Clean(abs)
	if abs != root && !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		return "", errors.New(`access denied: path "` + input + `" is outside workspace`)
	}
	return abs, nil
}
