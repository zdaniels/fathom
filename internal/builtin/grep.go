package builtin

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zdaniels/fathom/internal/agent"
)

// GrepTool walks the workspace and returns regex matches as
// {file, line, text} records. Skips node_modules, .git, build dirs, and
// binary-looking files (NUL-byte sniff).
func GrepTool() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "grep",
		Description: `Search the workspace for a regex pattern across all text files.
Returns matches as {file, line, text} records. Skips node_modules, .git, build/dist directories, and binary files.
Use to find usages, definitions, or any code containing a string. For file-name matching use glob instead.`,
		SkillName: "file-editor",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"pattern":    map[string]interface{}{"type": "string", "description": "Go RE2 regex (NOT PCRE). Use (?i) for case-insensitive."},
				"path":       map[string]interface{}{"type": "string", "description": "Directory to search under (default: workspace root)."},
				"include":    map[string]interface{}{"type": "string", "description": "Filename regex; only files matching are searched (e.g. `\\.go$`)."},
				"maxResults": map[string]interface{}{"type": "number", "description": "Cap total matches (default 100, max 500)."},
			},
			"required": []string{"pattern"},
		},
		Execute: func(ctx context.Context, p map[string]interface{}, tctx agent.ToolContext) (interface{}, error) {
			pattern, _ := p["pattern"].(string)
			dir, _ := p["path"].(string)
			includeStr, _ := p["include"].(string)
			max := 100
			if v, ok := p["maxResults"].(float64); ok && v > 0 {
				max = int(v)
				if max > 500 {
					max = 500
				}
			}
			re, err := regexp.Compile(pattern)
			if err != nil {
				return nil, fmt.Errorf("grep: invalid regex: %w", err)
			}
			var includeRe *regexp.Regexp
			if includeStr != "" {
				includeRe, err = regexp.Compile(includeStr)
				if err != nil {
					return nil, fmt.Errorf("grep: invalid include pattern: %w", err)
				}
			}
			root := workspaceRoot()
			start := root
			if dir != "" {
				start, err = resolveInWorkspace(dir)
				if err != nil {
					return nil, err
				}
			}
			if tctx.Policy != nil {
				if dec := tctx.Policy.CheckFilesystem(start, "read"); dec.Decision == "deny" {
					return nil, fmt.Errorf("grep blocked by policy: %s", dec.Reason)
				}
			}

			var hits []map[string]interface{}
			err = filepath.Walk(start, func(path string, info os.FileInfo, walkErr error) error {
				if walkErr != nil {
					return nil // skip unreadable
				}
				name := info.Name()
				if info.IsDir() {
					if name == "node_modules" || name == ".git" || name == "dist" ||
						name == "build" || name == "vendor" || name == "target" ||
						strings.HasPrefix(name, ".") && name != "." {
						return filepath.SkipDir
					}
					return nil
				}
				if includeRe != nil && !includeRe.MatchString(name) {
					return nil
				}
				// Cap file size to skip giant logs / binaries.
				if info.Size() > 4*1024*1024 {
					return nil
				}
				if len(hits) >= max {
					return filepath.SkipAll
				}
				f, err := os.Open(path)
				if err != nil {
					return nil
				}
				defer f.Close()
				// Binary sniff: read first 512 bytes, skip if it has NUL.
				peek := make([]byte, 512)
				n, _ := f.Read(peek)
				if containsNUL(peek[:n]) {
					return nil
				}
				_, _ = f.Seek(0, 0)
				scanner := bufio.NewScanner(f)
				scanner.Buffer(make([]byte, 64*1024), 1024*1024)
				lineNum := 0
				for scanner.Scan() {
					lineNum++
					text := scanner.Text()
					if re.MatchString(text) {
						rel, _ := filepath.Rel(root, path)
						hits = append(hits, map[string]interface{}{
							"file": rel,
							"line": lineNum,
							"text": strings.TrimRight(text, " \t"),
						})
						if len(hits) >= max {
							return filepath.SkipAll
						}
					}
				}
				return nil
			})
			if err != nil && err != filepath.SkipAll {
				return nil, fmt.Errorf("grep: %w", err)
			}
			return map[string]interface{}{
				"matches":   hits,
				"count":     len(hits),
				"truncated": len(hits) >= max,
			}, nil
		},
	}
}

func containsNUL(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}
