// Agent responses over SSE, forwarding provider text as it arrives.
package gateway

import (
	"encoding/json"
	"fmt"
	"github.com/zdaniels/fathom/internal/streamtext"
	"io"
	"net/http"
	"time"

	"github.com/zdaniels/fathom/pkg/types"
)

func (g *Gateway) handleStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	authResult, ok := g.authenticate(w, r)
	if !ok {
		return
	}

	if !g.executionAllowed(w, authResult.UserID) {
		return
	}
	// Need a flusher to push events as we generate them. http.ResponseWriter
	// gives us one when the underlying transport supports it (all stdlib
	// servers do).
	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, http.StatusInternalServerError, "streaming not supported by transport")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "body unreadable")
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.Text == "" {
		jsonError(w, http.StatusBadRequest, "missing 'text' field")
		return
	}

	g.mu.RLock()
	handler := g.msgHandler
	g.mu.RUnlock()
	if handler == nil {
		jsonError(w, http.StatusServiceUnavailable, "no message handler configured")
		return
	}

	// Set SSE headers BEFORE writing any body.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx/CF buffering
	w.WriteHeader(http.StatusOK)

	sess := g.Sessions.Create(authResult.UserID, remoteAddr(r))
	defer g.Sessions.Destroy(sess.ID)

	msg := types.ChannelMessage{
		ChannelType: "rest",
		ChannelID:   "stream",
		SenderID:    authResult.UserID,
		Text:        in.Text,
		Timestamp:   time.Now().UTC(),
	}

	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	flusher.Flush()
	emitted := false
	ctx := streamtext.With(r.Context(), func(delta string) {
		emitted = true
		writeSSEEvent(w, "message", map[string]string{"delta": delta})
		flusher.Flush()
	})
	reply, err := handler(ctx, msg, sess)
	if err != nil {
		writeSSEEvent(w, "error", map[string]string{"error": err.Error()})
		flusher.Flush()
		return
	}

	if !emitted {
		writeSSEEvent(w, "message", map[string]string{"delta": reply})
	}
	writeSSEEvent(w, "done", map[string]interface{}{
		"session_id": sess.ID,
		"reply":      reply,
	})
	flusher.Flush()
}

// writeSSEEvent writes one SSE record. SSE spec: each event is a series
// of `field: value\n` lines, terminated by a blank line. The browser's
// EventSource parses this into a `MessageEvent` it dispatches.
func writeSSEEvent(w io.Writer, event string, data interface{}) {
	raw, _ := json.Marshal(data)
	// `event:` is omitted for default-typed "message" events — that's
	// the spec default and lets the receiver use `evt.type === "message"`.
	if event != "" && event != "message" {
		_, _ = fmt.Fprintf(w, "event: %s\n", event)
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
}
