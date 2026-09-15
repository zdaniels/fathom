package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/zdaniels/fathom/internal/streamtext"
	"github.com/zdaniels/fathom/pkg/types"
)

// inlineToolCallTagRE matches a Hermes 3-style <tool_call>{...}</tool_call>
// block. (?s) lets . span newlines so we still catch multi-line JSON.
// We use ungreedy `.*?` so the regex stops at the first closing tag
// even when several tool_call blocks appear in one response.
var inlineToolCallTagRE = regexp.MustCompile(`(?s)<tool_call>\s*(\{.*?\})\s*</tool_call>`)

// Ollama implements Provider against a local Ollama instance. Used for
// fully-local deploys (no API key required). Tool calling support depends on
// the underlying model — gpt-oss, llama3.1+, qwen3, mistral-nemo, and
// command-r emit structured tool_calls; models without tool training will
// just emit text.
type Ollama struct {
	cfg     types.LLMConfig
	baseURL string
	client  *http.Client
}

func NewOllama(cfg types.LLMConfig) *Ollama {
	base := cfg.BaseURL
	if base == "" {
		base = "http://localhost:11434"
	}
	return &Ollama{cfg: cfg, baseURL: base, client: &http.Client{}}
}

func (o *Ollama) Chat(ctx context.Context, messages []Message, tools []ToolDef) (Response, error) {
	type olFuncCall struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments"`
	}
	type olToolCall struct {
		ID       string     `json:"id,omitempty"`
		Function olFuncCall `json:"function"`
	}
	type olMsg struct {
		Role       string       `json:"role"`
		Content    string       `json:"content"`
		ToolCallID string       `json:"tool_call_id,omitempty"`
		Name       string       `json:"name,omitempty"`
		ToolCalls  []olToolCall `json:"tool_calls,omitempty"`
	}
	type olToolFunc struct {
		Name        string                 `json:"name"`
		Description string                 `json:"description"`
		Parameters  map[string]interface{} `json:"parameters"`
	}
	type olTool struct {
		Type     string     `json:"type"`
		Function olToolFunc `json:"function"`
	}

	msgs := make([]olMsg, 0, len(messages))
	for _, m := range messages {
		om := olMsg{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		for _, tc := range m.ToolCalls {
			om.ToolCalls = append(om.ToolCalls, olToolCall{
				ID:       tc.ID,
				Function: olFuncCall{Name: tc.Name, Arguments: tc.Arguments},
			})
		}
		msgs = append(msgs, om)
	}
	body := map[string]interface{}{
		"model":    o.cfg.Model,
		"stream":   false,
		"messages": msgs,
	}
	if len(tools) > 0 {
		ot := make([]olTool, 0, len(tools))
		for _, t := range tools {
			ot = append(ot, olTool{
				Type: "function",
				Function: olToolFunc{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.Parameters,
				},
			})
		}
		body["tools"] = ot
	}

	if streamtext.Enabled(ctx) {
		body["stream"] = true
	}
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/chat", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		return Response{}, &ProviderError{Provider: "ollama", Msg: "request failed", Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK && streamtext.Enabled(ctx) {
		return ollamaStream(ctx, resp.Body, tools)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if resp.StatusCode != http.StatusOK {
		return Response{}, &ProviderError{Provider: "ollama",
			Msg: fmt.Sprintf("status %d: %s", resp.StatusCode, string(raw))}
	}

	// Ollama mirrors OpenAI's tool_calls shape, with one quirk: `arguments`
	// is a JSON OBJECT (not a JSON string as OpenAI emits). Decode straight
	// into map[string]interface{}.
	var parsed struct {
		Message struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string                 `json:"name"`
					Arguments map[string]interface{} `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls,omitempty"`
		} `json:"message"`
		DoneReason string `json:"done_reason"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Response{}, &ProviderError{Provider: "ollama", Msg: "response parse failed", Err: err}
	}

	out := Response{Content: parsed.Message.Content}
	for _, tc := range parsed.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%s_%d", tc.Function.Name, len(out.ToolCalls))
		}
		out.ToolCalls = append(out.ToolCalls, ToolCallRequest{
			ID:        id,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}

	// Inline-tool-call fallback. Models like Hermes 3 emit tool calls
	// as plain text in `content` instead of via the structured
	// `tool_calls` field — see the comment on extractInlineToolCalls.
	// Without this fallback those calls leak through to the user as
	// raw JSON ("{"name":"recall_search",...}") and the agent loop
	// never actually invokes the tool.
	if len(out.ToolCalls) == 0 && out.Content != "" && len(tools) > 0 {
		if inlineCalls, cleaned := extractInlineToolCalls(out.Content, tools); len(inlineCalls) > 0 {
			out.ToolCalls = inlineCalls
			out.Content = cleaned
		}
	}

	if len(out.ToolCalls) > 0 {
		out.FinishReason = FinishToolCalls
	} else {
		out.FinishReason = FinishStop
	}
	return out, nil
}

// extractInlineToolCalls scans `content` for tool-call JSON the model
// emitted inline rather than via the structured `tool_calls` field.
// Returns any matches plus the content with those blocks stripped, so
// the caller can keep any surrounding prose without the raw JSON
// bleeding through.
//
// Recognised forms:
//
//  1. <tool_call>{"name": "...", "arguments": {...}}</tool_call> —
//     Hermes 3's canonical inline format. Each match becomes one call.
//  2. The entire trimmed content IS the JSON tool call, no tags.
//     We accept this only when the parsed name matches a registered
//     tool, since plain `{...}` content is otherwise indistinguishable
//     from a code example or quoted JSON payload.
//
// `tools` gates form 2: the parsed name must appear in the offered
// tool set. When `tools` is empty we skip the inline parse entirely
// rather than synthesising calls that the agent loop can't route.
func extractInlineToolCalls(content string, tools []ToolDef) ([]ToolCallRequest, string) {
	valid := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		valid[t.Name] = struct{}{}
	}

	// Form 1: <tool_call>…</tool_call> tags.
	if matches := inlineToolCallTagRE.FindAllStringSubmatch(content, -1); len(matches) > 0 {
		var calls []ToolCallRequest
		for _, m := range matches {
			var tc struct {
				Name      string                 `json:"name"`
				Arguments map[string]interface{} `json:"arguments"`
			}
			if err := json.Unmarshal([]byte(m[1]), &tc); err != nil || tc.Name == "" {
				continue
			}
			if _, ok := valid[tc.Name]; !ok {
				// Unknown tool — skip rather than synthesise a call
				// the agent loop will then reject.
				continue
			}
			calls = append(calls, ToolCallRequest{
				ID:        fmt.Sprintf("call_%s_%d", tc.Name, len(calls)),
				Name:      tc.Name,
				Arguments: tc.Arguments,
			})
		}
		if len(calls) > 0 {
			cleaned := strings.TrimSpace(inlineToolCallTagRE.ReplaceAllString(content, ""))
			return calls, cleaned
		}
	}

	// Form 2: bare JSON object as the whole content.
	trimmed := strings.TrimSpace(content)
	if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
		var tc struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(trimmed), &tc); err == nil && tc.Name != "" {
			if _, ok := valid[tc.Name]; ok {
				return []ToolCallRequest{{
					ID:        fmt.Sprintf("call_%s_0", tc.Name),
					Name:      tc.Name,
					Arguments: tc.Arguments,
				}}, ""
			}
		}
	}

	return nil, content
}
