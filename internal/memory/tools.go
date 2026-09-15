package memory

import (
	"context"
	"encoding/json"

	"github.com/zdaniels/fathom/internal/agent"
)

// AsTools wraps the bridge as a set of ToolDefinitions the agent can call.
// The 8 recall MCP tools each map to one ToolDefinition; the agent loop
// invokes them like any other tool.
//
// We don't hard-code the tool schemas — recall publishes them via the MCP
// `tools/list` method. If recall isn't reachable, returns an empty slice.
func AsTools(ctx context.Context, b *Bridge) []agent.ToolDefinition {
	if b == nil {
		return nil
	}
	raw, err := b.Call(ctx, "tools/list", nil)
	if err != nil {
		return nil
	}
	var listing struct {
		Tools []struct {
			Name        string                 `json:"name"`
			Description string                 `json:"description"`
			InputSchema map[string]interface{} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		return nil
	}
	out := make([]agent.ToolDefinition, 0, len(listing.Tools))
	for _, t := range listing.Tools {
		name := t.Name
		desc := t.Description
		params := t.InputSchema
		out = append(out, agent.ToolDefinition{
			Name:        name,
			Description: "[memory] " + desc,
			Parameters:  params,
			SkillName:   "memory",
			Execute: func(ctx context.Context, p map[string]interface{}, _ agent.ToolContext) (interface{}, error) {
				raw, err := b.Call(ctx, "tools/call", map[string]interface{}{
					"name":      name,
					"arguments": p,
				})
				if err != nil {
					return nil, err
				}
				var result interface{}
				_ = json.Unmarshal(raw, &result)
				return result, nil
			},
		})
	}
	return out
}

// SessionInject calls recall's inject endpoint to build the agent-memory
// block for a new chat session. Returns "" if recall isn't reachable.
func SessionInject(ctx context.Context, b *Bridge, projectDir string) string {
	if b == nil {
		return ""
	}
	raw, err := b.Call(ctx, "inject", map[string]interface{}{
		"projectDir": projectDir,
	})
	if err != nil {
		return ""
	}
	var resp struct {
		Block string `json:"block"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return ""
	}
	return resp.Block
}
