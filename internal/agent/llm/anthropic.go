package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/zdaniels/fathom/internal/streamtext"
	"github.com/zdaniels/fathom/pkg/types"
)

// Anthropic implements Provider against the Messages API.
//
// Two structural differences vs OpenAI to keep in mind:
//   - System messages aren't a role; they go in the `system` top-level field.
//   - Tool results come back as "user" messages containing a tool_result
//     content block, not as a "tool" role.
type Anthropic struct {
	cfg    types.LLMConfig
	apiKey string
	client *http.Client
}

func NewAnthropic(cfg types.LLMConfig, apiKey string) *Anthropic {
	return &Anthropic{cfg: cfg, apiKey: apiKey, client: &http.Client{}}
}

func (a *Anthropic) Chat(ctx context.Context, messages []Message, tools []ToolDef) (Response, error) {
	// Split out the system message; wrap tool results in tool_result blocks.
	var systemText string
	type contentBlock map[string]interface{}
	type aMsg struct {
		Role    string         `json:"role"`
		Content []contentBlock `json:"content"`
	}
	conv := make([]aMsg, 0, len(messages))
	for _, m := range messages {
		if m.Role == "system" {
			if systemText != "" {
				systemText += "\n\n"
			}
			systemText += m.Content
			continue
		}
		if m.Role == "tool" {
			conv = append(conv, aMsg{
				Role: "user",
				Content: []contentBlock{{
					"type":        "tool_result",
					"tool_use_id": m.ToolCallID,
					"content":     m.Content,
				}},
			})
			continue
		}
		// Assistant turn carrying tool_use blocks. Anthropic returns these
		// inline with optional preceding text; we send them back the same
		// way so the model can correlate tool_result blocks that follow.
		var blocks []contentBlock
		if m.Content != "" {
			blocks = append(blocks, contentBlock{"type": "text", "text": m.Content})
		}
		for _, tc := range m.ToolCalls {
			blocks = append(blocks, contentBlock{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Name,
				"input": tc.Arguments,
			})
		}
		if len(blocks) == 0 {
			blocks = []contentBlock{{"type": "text", "text": ""}}
		}
		conv = append(conv, aMsg{Role: m.Role, Content: blocks})
	}

	body := map[string]interface{}{
		"model":      a.cfg.Model,
		"max_tokens": defaultInt(a.cfg.MaxTokens, 4096),
		"messages":   conv,
		"system":     systemText,
	}
	if len(tools) > 0 {
		atools := make([]map[string]interface{}, 0, len(tools))
		for _, t := range tools {
			atools = append(atools, map[string]interface{}{
				"name":         t.Name,
				"description":  t.Description,
				"input_schema": t.Parameters,
			})
		}
		body["tools"] = atools
	}

	if streamtext.Enabled(ctx) {
		body["stream"] = true
	}
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		withDefaultBase(a.cfg, "https://api.anthropic.com/v1").BaseURL+"/messages", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := a.client.Do(req)
	if err != nil {
		return Response{}, &ProviderError{Provider: "anthropic", Msg: "request failed", Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK && streamtext.Enabled(ctx) {
		return anthropicStream(ctx, resp.Body)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if resp.StatusCode != http.StatusOK {
		return Response{}, &ProviderError{Provider: "anthropic",
			Msg: fmt.Sprintf("status %d: %s", resp.StatusCode, string(raw))}
	}
	var parsed struct {
		Content []struct {
			Type  string                 `json:"type"`
			Text  string                 `json:"text,omitempty"`
			ID    string                 `json:"id,omitempty"`
			Name  string                 `json:"name,omitempty"`
			Input map[string]interface{} `json:"input,omitempty"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Response{}, &ProviderError{Provider: "anthropic", Msg: "response parse failed", Err: err}
	}
	var content string
	var calls []ToolCallRequest
	for _, b := range parsed.Content {
		switch b.Type {
		case "text":
			content += b.Text
		case "tool_use":
			calls = append(calls, ToolCallRequest{ID: b.ID, Name: b.Name, Arguments: b.Input})
		}
	}
	out := Response{Content: content, ToolCalls: calls}
	if parsed.Usage != nil {
		out.Usage = &Usage{PromptTokens: parsed.Usage.InputTokens, CompletionTokens: parsed.Usage.OutputTokens}
	}
	if parsed.StopReason == "tool_use" {
		out.FinishReason = FinishToolCalls
	} else {
		out.FinishReason = FinishStop
	}
	return out, nil
}
