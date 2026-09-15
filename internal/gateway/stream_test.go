package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zdaniels/fathom/pkg/types"
)

func TestNonStreamingHandlerEmitsOneHonestDelta(t *testing.T) {
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
	if c := strings.Count(body, "\"delta\":"); c != 1 {
		t.Errorf("got %d deltas from nonstreaming handler", c)
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
