package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/zdaniels/fathom/pkg/types"
)

// Gemini implements Provider against Google's Generative Language API
// (a.k.a. Google AI Studio / Gemini API — not the Vertex AI variant).
//
// Three shape differences vs OpenAI/Anthropic worth keeping in mind:
//   - Roles are "user" and "model" — there is no "assistant" or "tool".
//   - System prompts go in a top-level `systemInstruction` field.
//   - Tool calls/results are content "parts": `functionCall {name, args}`
//     when the model invokes one, `functionResponse {name, response}` when
//     we hand back the result. Both ride inside a regular content turn.
type Gemini struct {
	cfg     types.LLMConfig
	apiKey  string
	baseURL string
	client  *http.Client
}

func NewGemini(cfg types.LLMConfig, apiKey string) *Gemini {
	base := cfg.BaseURL
	if base == "" {
		base = "https://generativelanguage.googleapis.com/v1beta"
	}
	return &Gemini{cfg: cfg, apiKey: apiKey, baseURL: base, client: &http.Client{}}
}

func (g *Gemini) Chat(ctx context.Context, messages []Message, tools []ToolDef) (Response, error) {
	type gPart map[string]interface{}
	type gContent struct {
		Role  string  `json:"role"`
		Parts []gPart `json:"parts"`
	}

	var systemText string
	conv := make([]gContent, 0, len(messages))

	for _, m := range messages {
		switch m.Role {
		case "system":
			if systemText != "" {
				systemText += "\n\n"
			}
			systemText += m.Content
			continue
		case "tool":
			// Tool results in Gemini are functionResponse parts inside a
			// user-role turn. The wire format wants `response` to be an
			// object, so wrap plain strings.
			resp := map[string]interface{}{"content": m.Content}
			conv = append(conv, gContent{
				Role: "user",
				Parts: []gPart{{
					"functionResponse": map[string]interface{}{
						"name":     m.Name,
						"response": resp,
					},
				}},
			})
			continue
		}

		// "assistant" → "model" in Gemini's vocabulary.
		role := m.Role
		if role == "assistant" {
			role = "model"
		}

		var parts []gPart
		if m.Content != "" {
			parts = append(parts, gPart{"text": m.Content})
		}
		for _, tc := range m.ToolCalls {
			args := tc.Arguments
			if args == nil {
				args = map[string]interface{}{}
			}
			parts = append(parts, gPart{
				"functionCall": map[string]interface{}{
					"name": tc.Name,
					"args": args,
				},
			})
		}
		if len(parts) == 0 {
			parts = []gPart{{"text": ""}}
		}
		conv = append(conv, gContent{Role: role, Parts: parts})
	}

	body := map[string]interface{}{
		"contents": conv,
		"generationConfig": map[string]interface{}{
			"maxOutputTokens": defaultInt(g.cfg.MaxTokens, 4096),
			"temperature":     defaultFloat(g.cfg.Temperature, 0.7),
		},
	}
	if systemText != "" {
		body["systemInstruction"] = map[string]interface{}{
			"parts": []gPart{{"text": systemText}},
		}
	}
	if len(tools) > 0 {
		decls := make([]map[string]interface{}, 0, len(tools))
		for _, t := range tools {
			decls = append(decls, map[string]interface{}{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
			})
		}
		body["tools"] = []map[string]interface{}{{"functionDeclarations": decls}}
	}

	buf, _ := json.Marshal(body)
	endpoint := fmt.Sprintf("%s/models/%s:generateContent?key=%s",
		g.baseURL, url.PathEscape(g.cfg.Model), url.QueryEscape(g.apiKey))
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return Response{}, &ProviderError{Provider: "gemini", Msg: "request failed", Err: err}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if resp.StatusCode != http.StatusOK {
		return Response{}, &ProviderError{Provider: "gemini",
			Msg: fmt.Sprintf("status %d: %s", resp.StatusCode, string(raw))}
	}

	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string `json:"text,omitempty"`
					FunctionCall *struct {
						Name string                 `json:"name"`
						Args map[string]interface{} `json:"args"`
					} `json:"functionCall,omitempty"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata *struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Response{}, &ProviderError{Provider: "gemini", Msg: "response parse failed", Err: err}
	}
	if len(parsed.Candidates) == 0 {
		return Response{}, &ProviderError{Provider: "gemini", Msg: "no candidates returned"}
	}
	cand := parsed.Candidates[0]
	var content string
	var calls []ToolCallRequest
	for _, p := range cand.Content.Parts {
		if p.Text != "" {
			content += p.Text
		}
		if p.FunctionCall != nil {
			// Gemini doesn't mint IDs for tool calls — synthesize one so the
			// rest of the agent loop can correlate request/result pairs the
			// same way it does for OpenAI/Anthropic.
			id := fmt.Sprintf("call_%s_%d", p.FunctionCall.Name, len(calls))
			calls = append(calls, ToolCallRequest{
				ID:        id,
				Name:      p.FunctionCall.Name,
				Arguments: p.FunctionCall.Args,
			})
		}
	}

	out := Response{Content: content, ToolCalls: calls}
	if parsed.UsageMetadata != nil {
		out.Usage = &Usage{
			PromptTokens:     parsed.UsageMetadata.PromptTokenCount,
			CompletionTokens: parsed.UsageMetadata.CandidatesTokenCount,
		}
	}
	if len(calls) > 0 {
		out.FinishReason = FinishToolCalls
	} else {
		switch cand.FinishReason {
		case "MAX_TOKENS":
			out.FinishReason = FinishLength
		default:
			out.FinishReason = FinishStop
		}
	}
	return out, nil
}
