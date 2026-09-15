package builtin

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/zdaniels/fathom/internal/agent"
)

// EditFileTool does a targeted string replacement inside an existing file.
// Unique old_string must exist; new_string replaces it. Refuses ambiguous
// matches — the model has to disambiguate by providing a larger surrounding
// context if the change site occurs more than once.
//
// Why this and not just write_file: whole-file writes lose history on every
// turn, are easy to mis-format, and burn tokens. Targeted edits are surgical
// and the model can reason about them precisely.
func EditFileTool() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "edit_file",
		Description: `Apply a targeted edit to an existing file by replacing one occurrence of old_string with new_string.
old_string must appear EXACTLY ONCE in the file. If it appears multiple times, include enough surrounding context to make it unique, or use replaceAll=true to swap every match.
For new files use write_file. For multiple edits in one file, call edit_file multiple times.`,
		SkillName: "file-editor",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":       map[string]interface{}{"type": "string", "description": "File path within the workspace."},
				"old_string": map[string]interface{}{"type": "string", "description": "Exact text to find. Must be unique unless replaceAll=true."},
				"new_string": map[string]interface{}{"type": "string", "description": "Replacement text. May be empty to delete the matched region."},
				"replaceAll": map[string]interface{}{"type": "boolean", "description": "When true, every occurrence of old_string is replaced. Default false."},
			},
			"required": []string{"path", "old_string", "new_string"},
		},
		Execute: func(ctx context.Context, p map[string]interface{}, tctx agent.ToolContext) (interface{}, error) {
			path, _ := p["path"].(string)
			oldS, _ := p["old_string"].(string)
			newS, _ := p["new_string"].(string)
			replaceAll, _ := p["replaceAll"].(bool)
			if oldS == "" {
				return nil, fmt.Errorf("edit_file: old_string must not be empty (use write_file for new files)")
			}
			if oldS == newS {
				return nil, fmt.Errorf("edit_file: old_string and new_string are identical — no edit needed")
			}
			abs, err := resolveInWorkspace(path)
			if err != nil {
				return nil, err
			}
			if tctx.Policy != nil {
				if dec := tctx.Policy.CheckFilesystem(abs, "write"); dec.Decision == "deny" {
					return nil, fmt.Errorf("edit_file blocked by policy: %s", dec.Reason)
				}
			}
			data, err := os.ReadFile(abs)
			if err != nil {
				return nil, fmt.Errorf("edit_file: %w", err)
			}
			body := string(data)
			count := strings.Count(body, oldS)
			if count == 0 {
				return nil, fmt.Errorf("edit_file: old_string not found in %s", path)
			}
			if count > 1 && !replaceAll {
				return nil, fmt.Errorf("edit_file: old_string occurs %d times in %s — provide more surrounding context to disambiguate, or set replaceAll=true", count, path)
			}
			var out string
			var replaced int
			if replaceAll {
				out = strings.ReplaceAll(body, oldS, newS)
				replaced = count
			} else {
				out = strings.Replace(body, oldS, newS, 1)
				replaced = 1
			}
			if err := os.WriteFile(abs, []byte(out), 0o644); err != nil {
				return nil, fmt.Errorf("edit_file: %w", err)
			}
			return map[string]interface{}{
				"ok":           true,
				"path":         path,
				"replacements": replaced,
			}, nil
		},
	}
}
