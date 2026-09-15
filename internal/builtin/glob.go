package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zdaniels/fathom/internal/agent"
)

// GlobTool finds files matching a doublestar-style pattern under the
// workspace. Supports `**` for "any number of directories" — filepath.Match
// alone doesn't.
//
// Returns paths sorted by modification time (newest first), so the model
// sees the most recently-touched files at the top — often the working set.
func GlobTool() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "glob",
		Description: `Find files by name pattern. Supports ** for recursive match. Returns paths sorted by modification time (newest first).
Examples: "**/*.go" — all Go files; "internal/**/*_test.go" — tests under internal/; "*.md" — markdown in root.
For content search use grep.`,
		SkillName: "file-editor",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"pattern": map[string]interface{}{
					"type":        "string",
					"description": "Glob pattern. `**` matches zero or more dirs; `*` matches within a name.",
				},
				"maxResults": map[string]interface{}{
					"type":        "number",
					"description": "Cap results (default 100, max 500).",
				},
			},
			"required": []string{"pattern"},
		},
		Execute: func(ctx context.Context, p map[string]interface{}, tctx agent.ToolContext) (interface{}, error) {
			pattern, _ := p["pattern"].(string)
			if strings.TrimSpace(pattern) == "" {
				return nil, fmt.Errorf("glob: pattern required")
			}
			max := 100
			if v, ok := p["maxResults"].(float64); ok && v > 0 {
				max = int(v)
				if max > 500 {
					max = 500
				}
			}
			root := workspaceRoot()
			if tctx.Policy != nil {
				if dec := tctx.Policy.CheckFilesystem(root, "read"); dec.Decision == "deny" {
					return nil, fmt.Errorf("glob blocked by policy: %s", dec.Reason)
				}
			}

			type entry struct {
				Path    string
				ModTime int64
			}
			var matches []entry
			err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
				if walkErr != nil {
					return nil
				}
				name := info.Name()
				if info.IsDir() {
					if name == "node_modules" || name == ".git" || name == "dist" ||
						name == "build" || name == "vendor" || name == "target" {
						return filepath.SkipDir
					}
					return nil
				}
				rel, _ := filepath.Rel(root, path)
				if matched := doublestarMatch(pattern, rel); matched {
					matches = append(matches, entry{Path: rel, ModTime: info.ModTime().Unix()})
				}
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("glob: %w", err)
			}
			sort.Slice(matches, func(i, j int) bool { return matches[i].ModTime > matches[j].ModTime })
			if len(matches) > max {
				matches = matches[:max]
			}
			paths := make([]string, len(matches))
			for i, m := range matches {
				paths[i] = m.Path
			}
			return map[string]interface{}{
				"files":     paths,
				"count":     len(paths),
				"truncated": len(matches) == max,
			}, nil
		},
	}
}

// doublestarMatch is filepath.Match with one extension: `**` matches zero
// or more path components. Implementation: split pattern by `/`, recurse.
func doublestarMatch(pattern, path string) bool {
	patParts := strings.Split(pattern, "/")
	pathParts := strings.Split(path, "/")
	return matchParts(patParts, pathParts)
}

func matchParts(pat, path []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			if len(rest) == 0 {
				return true // trailing ** matches anything
			}
			for i := 0; i <= len(path); i++ {
				if matchParts(rest, path[i:]) {
					return true
				}
			}
			return false
		}
		if len(path) == 0 {
			return false
		}
		ok, _ := filepath.Match(pat[0], path[0])
		if !ok {
			return false
		}
		pat = pat[1:]
		path = path[1:]
	}
	return len(path) == 0
}
