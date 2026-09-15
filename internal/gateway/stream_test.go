package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zdaniels/fathom/pkg/types"
)

func TestStreamSendsSSEChunks(t *testing.T) {
	g, tok := newTestGateway(t)
	defer g.Pairing.Close()
	g.SetMessageHandler(func(ctx context.Context, msg types.ChannelMessage, _ types.Session) (string, error) {
		return "hello there friend, this is a streaming reply with several chunks", nil
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/stream",
		strings.NewReader(`{"text":"hi"}`))
	r.Header.Set("Authorization", "Bearer "+tok)
	g.handleStream(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, "data: {\"delta\":") {
		t.Errorf("body missing data: chunks. Body:\n%s", body)
	}
	if !strings.Contains(body, "event: done") {
		t.Errorf("body missing done event. Body:\n%s", body)
	}
	// Sanity check: chunk count > 1 (the chunker should have split the
	// reply into more than one piece).
	if c := strings.Count(body, "\"delta\":"); c < 2 {
		t.Errorf("delta chunk count = %d, want >= 2 for a multi-word reply", c)
	}
}

func TestStreamRequiresAuth(t *testing.T) {
	g, _ := newTestGateway(t)
	defer g.Pairing.Close()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/stream",
		strings.NewReader(`{"text":"hi"}`))
	g.handleStream(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestChunkTextPrefersWordBoundaries(t *testing.T) {
	// Internal helper — exercise it directly so a future change to
	// chunk-size logic gets caught.
	text := "alpha beta gamma delta epsilon zeta eta theta iota kappa"
	chunks := chunkText(text, 12)
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want >= 2", len(chunks))
	}

	// Invariant 1: chunks rejoin losslessly. No chars dropped or duped.
	if rejoined := strings.Join(chunks, ""); rejoined != text {
		t.Errorf("rejoin mismatch:\n  got:  %q\n  want: %q", rejoined, text)
	}

	// Invariant 2: no chunk boundary cuts a word in half. A cut is
	// mid-word when chunk[N] ends in a letter AND chunk[N+1] starts
	// with a letter (no whitespace either side). chunkText puts the
	// whitespace on the leading side of the next chunk, so the next
	// chunk's first char is the indicator.
	for i := 0; i < len(chunks)-1; i++ {
		next := chunks[i+1]
		if len(next) == 0 {
			continue
		}
		if !strings.ContainsAny(string(next[0]), " \n\t") {
			t.Errorf("boundary between chunk %d and %d cuts a word: %q | %q",
				i, i+1, chunks[i], next)
		}
	}
}
