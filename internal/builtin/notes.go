// Package builtin holds the starter skills inlined as Go-side ToolDefinitions.
// They run in-process (no sandbox subprocess) — fine for personal mode where
// the host IS the trust boundary. notes / web-search / file-editor are
// enabled out of the box so a fresh `fathom chat` has something to do.
package builtin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zdaniels/fathom/internal/agent"
)

// Note is a single record in the local notes store. In-memory + process-
// scoped — Phase 2 moves this to encrypted SQLite under @fathom/memory.
type Note struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Content   string    `json:"content"`
	Tags      []string  `json:"tags"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

var (
	notesMu    sync.RWMutex
	notesStore = make(map[string]Note)
)

// NotesTools returns the three notes ToolDefinitions ready to register.
func NotesTools() []agent.ToolDefinition {
	return []agent.ToolDefinition{
		{
			Name:        "create_note",
			Description: "Create a new note",
			SkillName:   "notes",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"title":   map[string]interface{}{"type": "string", "description": "Note title"},
					"content": map[string]interface{}{"type": "string", "description": "Note content"},
					"tags":    map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
				},
				"required": []string{"title", "content"},
			},
			Execute: func(ctx context.Context, p map[string]interface{}, _ agent.ToolContext) (interface{}, error) {
				title, _ := p["title"].(string)
				content, _ := p["content"].(string)
				tags := toStringSlice(p["tags"])
				id := randomID()
				now := time.Now().UTC()
				note := Note{ID: id, Title: title, Content: content, Tags: tags, CreatedAt: now, UpdatedAt: now}
				notesMu.Lock()
				notesStore[id] = note
				notesMu.Unlock()
				return map[string]string{"id": id}, nil
			},
		},
		{
			Name:        "search_notes",
			Description: "Search notes by full-text query",
			SkillName:   "notes",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"query": map[string]interface{}{"type": "string"}},
				"required":   []string{"query"},
			},
			Execute: func(ctx context.Context, p map[string]interface{}, _ agent.ToolContext) (interface{}, error) {
				q, _ := p["query"].(string)
				ql := strings.ToLower(q)
				var hits []Note
				notesMu.RLock()
				for _, n := range notesStore {
					if strings.Contains(strings.ToLower(n.Title), ql) || strings.Contains(strings.ToLower(n.Content), ql) {
						hits = append(hits, n)
					}
				}
				notesMu.RUnlock()
				return map[string]interface{}{"notes": hits}, nil
			},
		},
		{
			Name:        "list_notes",
			Description: "List notes, optionally filtered by tag, most recently updated first",
			SkillName:   "notes",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"tag":   map[string]interface{}{"type": "string"},
					"limit": map[string]interface{}{"type": "number", "description": "Max results (default 50)"},
				},
				"required": []string{},
			},
			Execute: func(ctx context.Context, p map[string]interface{}, _ agent.ToolContext) (interface{}, error) {
				tag, _ := p["tag"].(string)
				limit := 50
				if v, ok := p["limit"].(float64); ok {
					limit = int(v)
				}
				notesMu.RLock()
				all := make([]Note, 0, len(notesStore))
				for _, n := range notesStore {
					if tag != "" {
						hasTag := false
						for _, t := range n.Tags {
							if strings.EqualFold(t, tag) {
								hasTag = true
								break
							}
						}
						if !hasTag {
							continue
						}
					}
					all = append(all, n)
				}
				notesMu.RUnlock()
				sort.Slice(all, func(i, j int) bool { return all[i].UpdatedAt.After(all[j].UpdatedAt) })
				if len(all) > limit {
					all = all[:limit]
				}
				return map[string]interface{}{"notes": all}, nil
			},
		},
	}
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func toStringSlice(v interface{}) []string {
	switch t := v.(type) {
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	}
	return nil
}

// _ silences unused import errors during partial builds.
var _ = fmt.Sprintf
