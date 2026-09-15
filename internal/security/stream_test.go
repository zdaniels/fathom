package security

import (
	"context"
	"github.com/zdaniels/fathom/internal/streamtext"
	"strings"
	"testing"
)

func TestStreamCanaryAcrossDeltas(t *testing.T) {
	c := NewCanarySystem()
	token := c.Issue("private")
	var output strings.Builder
	ctx, flush := c.FilterStream(streamtext.With(context.Background(), func(s string) { output.WriteString(s) }))
	streamtext.Emit(ctx, strings.Repeat("public ", 30))
	if output.Len() == 0 {
		t.Fatal("safe text was fully buffered")
	}
	for _, r := range token {
		streamtext.Emit(ctx, string(r))
	}
	streamtext.Emit(ctx, "after canary")
	flush()
	if strings.Contains(output.String(), "ICLW") || strings.Contains(output.String(), "after canary") {
		t.Fatal("canary or following text leaked")
	}
}
