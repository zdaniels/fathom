package llm

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/zdaniels/fathom/internal/streamtext"
)

func TestOpenAIStreamsBeforeCompletionAndAssemblesTools(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	text := make(chan string, 1)
	finished := make(chan error, 1)
	ctx := streamtext.With(context.Background(), func(delta string) { text <- delta })
	go func() {
		out, err := openAIStream(ctx, reader)
		if err == nil && (out.Content != "Hello" || len(out.ToolCalls) != 1 || out.ToolCalls[0].Arguments["file"] != "a.go") {
			t.Errorf("unexpected response: %+v", out)
		}
		finished <- err
	}()
	_, _ = io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n")
	select {
	case got := <-text:
		if got != "Hello" {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("text buffered until completion")
	}
	_, _ = io.WriteString(writer, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call1","function":{"name":"read","arguments":"{\"file\":"}}]}}]}`+"\n\n"+`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n"+"data: [DONE]\n\n")
	writer.Close()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}
func TestProviderStreamsRejectTruncation(t *testing.T) {
	for _, read := range []func(context.Context, io.Reader) (Response, error){openAIStream, anthropicStream, func(ctx context.Context, r io.Reader) (Response, error) { return ollamaStream(ctx, r, nil) }} {
		if _, err := read(context.Background(), strings.NewReader("")); err == nil {
			t.Fatal("truncated stream accepted")
		}
	}
}
func TestAnthropicStreamDoesNotExposeThinking(t *testing.T) {
	var text strings.Builder
	ctx := streamtext.With(context.Background(), func(s string) { text.WriteString(s) })
	wire := `data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"private"}}

data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"public"}}

data: {"type":"message_stop"}

`
	out, err := anthropicStream(ctx, strings.NewReader(wire))
	if err != nil || out.Content != "public" || text.String() != "public" {
		t.Fatalf("%+v %v %q", out, err, text.String())
	}
}
func TestOllamaStreamToolsAndUsage(t *testing.T) {
	wire := `{"message":{"content":"Checking","tool_calls":[{"function":{"name":"shell","arguments":{"command":"ls"}}}]}}
{"done":true,"prompt_eval_count":5,"eval_count":8}
`
	out, err := ollamaStream(context.Background(), strings.NewReader(wire), nil)
	if err != nil || len(out.ToolCalls) != 1 || out.Usage.CompletionTokens != 8 || out.FinishReason != FinishToolCalls {
		t.Fatalf("%+v %v", out, err)
	}
}
