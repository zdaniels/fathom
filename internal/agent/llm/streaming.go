package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/zdaniels/fathom/internal/streamtext"
)

// readEvents bounds total response and individual event size, and supports
// multi-line SSE data fields. Unexpected EOF is handled by each provider.
func readEvents(r io.Reader, fn func([]byte) error) error {
	scanner := bufio.NewScanner(io.LimitReader(r, 8<<20))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var data []string
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		b := []byte(strings.Join(data, "\n"))
		data = nil
		return fn(b)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}

type streamedCall struct{ ID, Name, Arguments string }

func openAIStream(ctx context.Context, r io.Reader) (Response, error) {
	out := Response{}
	calls := map[int]*streamedCall{}
	done := false
	err := readEvents(r, func(b []byte) error {
		if string(b) == "[DONE]" {
			done = true
			return nil
		}
		var e struct {
			Error   json.RawMessage
			Choices []struct {
				Index int
				Delta struct {
					Content   string
					ToolCalls []struct {
						Index    int
						ID       string
						Function struct{ Name, Arguments string }
					} `json:"tool_calls"`
				}
				FinishReason string `json:"finish_reason"`
			}
			Usage *struct {
				Prompt     int `json:"prompt_tokens"`
				Completion int `json:"completion_tokens"`
			}
		}
		if err := json.Unmarshal(b, &e); err != nil {
			return err
		}
		if len(e.Error) > 0 && string(e.Error) != "null" {
			return errors.New("provider returned a streaming error")
		}
		if e.Usage != nil {
			out.Usage = &Usage{PromptTokens: e.Usage.Prompt, CompletionTokens: e.Usage.Completion}
		}
		for _, ch := range e.Choices {
			if ch.Index != 0 {
				continue
			}
			out.Content += ch.Delta.Content
			streamtext.Emit(ctx, ch.Delta.Content)
			if ch.FinishReason != "" {
				out.FinishReason = FinishReason(ch.FinishReason)
			}
			for _, tc := range ch.Delta.ToolCalls {
				if tc.Index < 0 || tc.Index > 127 {
					return errors.New("invalid tool index")
				}
				c := calls[tc.Index]
				if c == nil {
					c = &streamedCall{}
					calls[tc.Index] = c
				}
				c.ID += tc.ID
				c.Name += tc.Function.Name
				c.Arguments += tc.Function.Arguments
			}
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	if !done {
		return out, io.ErrUnexpectedEOF
	}
	indices := []int{}
	for i := range calls {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	for _, i := range indices {
		c := calls[i]
		var args map[string]interface{}
		if err = json.Unmarshal([]byte(c.Arguments), &args); err != nil {
			return out, fmt.Errorf("invalid streamed tool arguments: %w", err)
		}
		out.ToolCalls = append(out.ToolCalls, ToolCallRequest{ID: c.ID, Name: c.Name, Arguments: args})
	}
	return out, nil
}
func anthropicStream(ctx context.Context, r io.Reader) (Response, error) {
	out := Response{Usage: &Usage{}}
	calls := map[int]*streamedCall{}
	done := false
	err := readEvents(r, func(b []byte) error {
		var e struct {
			Type    string
			Index   int
			Message struct {
				Usage struct {
					Input  int `json:"input_tokens"`
					Output int `json:"output_tokens"`
				}
			}
			ContentBlock struct {
				Type, Text, ID, Name string
				Input                json.RawMessage
			} `json:"content_block"`
			Delta struct {
				Type, Text  string
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			}
			Usage struct {
				Output int `json:"output_tokens"`
			}
		}
		if err := json.Unmarshal(b, &e); err != nil {
			return err
		}
		switch e.Type {
		case "error":
			return errors.New("Anthropic returned a streaming error")
		case "message_start":
			out.Usage.PromptTokens = e.Message.Usage.Input
			out.Usage.CompletionTokens = e.Message.Usage.Output
		case "content_block_start":
			if e.Index < 0 || e.Index > 127 {
				return errors.New("invalid content index")
			}
			if e.ContentBlock.Type == "tool_use" {
				calls[e.Index] = &streamedCall{ID: e.ContentBlock.ID, Name: e.ContentBlock.Name}
			} else if e.ContentBlock.Type == "text" {
				out.Content += e.ContentBlock.Text
				streamtext.Emit(ctx, e.ContentBlock.Text)
			}
		case "content_block_delta":
			if e.Delta.Type == "text_delta" {
				out.Content += e.Delta.Text
				streamtext.Emit(ctx, e.Delta.Text)
			} else if e.Delta.Type == "input_json_delta" {
				c := calls[e.Index]
				if c == nil {
					return errors.New("tool delta without start")
				}
				c.Arguments += e.Delta.PartialJSON
			}
		case "message_delta":
			out.Usage.CompletionTokens = e.Usage.Output
			switch e.Delta.StopReason {
			case "tool_use":
				out.FinishReason = FinishToolCalls
			case "max_tokens":
				out.FinishReason = FinishLength
			default:
				out.FinishReason = FinishStop
			}
		case "message_stop":
			done = true
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	if !done {
		return out, io.ErrUnexpectedEOF
	}
	indices := []int{}
	for i := range calls {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	for _, i := range indices {
		c := calls[i]
		if c.Arguments == "" {
			c.Arguments = "{}"
		}
		var args map[string]interface{}
		if err = json.Unmarshal([]byte(c.Arguments), &args); err != nil {
			return out, err
		}
		out.ToolCalls = append(out.ToolCalls, ToolCallRequest{ID: c.ID, Name: c.Name, Arguments: args})
	}
	return out, nil
}
func ollamaStream(ctx context.Context, r io.Reader, tools []ToolDef) (Response, error) {
	out := Response{}
	scanner := bufio.NewScanner(io.LimitReader(r, 8<<20))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	done := false
	for scanner.Scan() {
		var e struct {
			Error      string
			Done       bool
			DoneReason string `json:"done_reason"`
			Prompt     int    `json:"prompt_eval_count"`
			Completion int    `json:"eval_count"`
			Message    struct {
				Content   string
				ToolCalls []struct {
					ID       string
					Function struct {
						Name      string
						Arguments map[string]interface{}
					}
				} `json:"tool_calls"`
			}
		}
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			return out, err
		}
		if e.Error != "" {
			return out, errors.New("Ollama returned a streaming error")
		}
		out.Content += e.Message.Content
		streamtext.Emit(ctx, e.Message.Content)
		for _, tc := range e.Message.ToolCalls {
			id := tc.ID
			if id == "" {
				id = fmt.Sprintf("call_%s_%d", tc.Function.Name, len(out.ToolCalls))
			}
			out.ToolCalls = append(out.ToolCalls, ToolCallRequest{ID: id, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
		}
		if e.Done {
			done = true
			out.Usage = &Usage{PromptTokens: e.Prompt, CompletionTokens: e.Completion}
			if e.DoneReason == "length" {
				out.FinishReason = FinishLength
			} else {
				out.FinishReason = FinishStop
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return out, err
	}
	if !done {
		return out, io.ErrUnexpectedEOF
	}
	if len(out.ToolCalls) == 0 && len(tools) > 0 {
		if calls, text := extractInlineToolCalls(out.Content, tools); len(calls) > 0 {
			out.ToolCalls = calls
			out.Content = text
		}
	}
	if len(out.ToolCalls) > 0 {
		out.FinishReason = FinishToolCalls
	}
	return out, nil
}
