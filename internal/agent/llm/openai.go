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

// OpenAI implements Provider against the Chat Completions API.
type OpenAI struct {
	cfg     types.LLMConfig
	apiKey  string
	baseURL string
	client  *http.Client
}

func NewOpenAI(cfg types.LLMConfig, apiKey string) *OpenAI {
	base := cfg.BaseURL
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	return &OpenAI{cfg: cfg, apiKey: apiKey, baseURL: base, client: &http.Client{}}
}

func (o *OpenAI) Chat(ctx context.Context, messages []Message, tools []ToolDef) (Response, error) {
	type oaFuncCall struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // OpenAI quirk: stringified JSON
	}
	type oaToolCall struct {
		ID       string     `json:"id"`
		Type     string     `json:"type"`
		Function oaFuncCall `json:"function"`
	}
	type oaMsg struct {
		Role       string       `json:"role"`
		Content    string       `json:"content"`
		Name       string       `json:"name,omitempty"`
		ToolCallID string       `json:"tool_call_id,omitempty"`
		ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	}
	type oaToolFn struct {
		Name        string                 `json:"name"`
		Description string                 `json:"description"`
		Parameters  map[string]interface{} `json:"parameters"`
	}
	type oaTool struct {
		Type     string   `json:"type"`
		Function oaToolFn `json:"function"`
	}
	body := map[string]interface{}{
		"model":       o.cfg.Model,
		"messages":    []oaMsg{},
		"max_tokens":  defaultInt(o.cfg.MaxTokens, 4096),
		"temperature": defaultFloat(o.cfg.Temperature, 0.7),
	}
	msgs := make([]oaMsg, 0, len(messages))
	for _, m := range messages {
		om := oaMsg{Role: m.Role, Content: m.Content, Name: m.Name, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			argsJSON, _ := json.Marshal(tc.Arguments)
			om.ToolCalls = append(om.ToolCalls, oaToolCall{
				ID:       tc.ID,
				Type:     "function",
				Function: oaFuncCall{Name: tc.Name, Arguments: string(argsJSON)},
			})
		}
		msgs = append(msgs, om)
	}
	body["messages"] = msgs
	if len(tools) > 0 {
		ts := make([]oaTool, 0, len(tools))
		for _, t := range tools {
			ts = append(ts, oaTool{Type: "function", Function: oaToolFn{
				Name: t.Name, Description: t.Description, Parameters: t.Parameters,
			}})
		}
		body["tools"] = ts
	}

	if streamtext.Enabled(ctx) {
		body["stream"] = true
	}
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.apiKey)
	resp, err := o.client.Do(req)
	if err != nil {
		return Response{}, &ProviderError{Provider: "openai", Msg: "request failed", Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK && streamtext.Enabled(ctx) {
		return openAIStream(ctx, resp.Body)
	}
	rawResp, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if resp.StatusCode != http.StatusOK {
		return Response{}, &ProviderError{Provider: "openai", Msg: fmt.Sprintf("status %d: %s", resp.StatusCode, string(rawResp))}
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rawResp, &parsed); err != nil {
		return Response{}, &ProviderError{Provider: "openai", Msg: "response parse failed", Err: err}
	}
	if len(parsed.Choices) == 0 {
		return Response{}, &ProviderError{Provider: "openai", Msg: "no choices returned"}
	}
	choice := parsed.Choices[0]
	out := Response{Content: choice.Message.Content}
	for _, tc := range choice.Message.ToolCalls {
		var args map[string]interface{}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		out.ToolCalls = append(out.ToolCalls, ToolCallRequest{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: args,
		})
	}
	if parsed.Usage != nil {
		out.Usage = &Usage{PromptTokens: parsed.Usage.PromptTokens, CompletionTokens: parsed.Usage.CompletionTokens}
	}
	if choice.FinishReason == "tool_calls" {
		out.FinishReason = FinishToolCalls
	} else {
		out.FinishReason = FinishStop
	}
	return out, nil
}

func defaultInt(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}
func defaultFloat(v, d float32) float32 {
	if v == 0 {
		return d
	}
	return v
}
