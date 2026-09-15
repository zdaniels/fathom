package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/zdaniels/fathom/internal/streamtext"
)

func TestClaudePartialTextAndFinalRecord(t *testing.T) {
	var text strings.Builder
	w := &claudeStreamWriter{ctx: streamtext.With(context.Background(), func(s string) { text.WriteString(s) })}
	chunks := []string{`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hel`, "lo\"}}}\n", `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"hidden"}}}` + "\n", `{"type":"result","subtype":"success","result":"hello","session_id":"resume-me"}` + "\n"}
	for _, s := range chunks {
		if _, err := w.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	r, err := parseClaudeResult(CodingAgentOptions{}, w.result, nil, nil)
	if err != nil || r.SessionID != "resume-me" || text.String() != "hello" {
		t.Fatalf("%+v %v %q", r, err, text.String())
	}
}
