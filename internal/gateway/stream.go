// Gateway HTTP handler for /api/v1/stream — Server-Sent Events delivery
// of agent responses.
//
// **Honest note on v1 implementation:** real per-token streaming
// requires upstream changes to the llm.Provider interface (a Stream
// method on each of OpenAI / Anthropic / Ollama; all three providers'
// APIs natively support it, our wrapper just doesn't expose it yet).
// That's its own focused PR.
//
// For now this endpoint waits for the full agent reply from the existing
// handler, then chunks it across the wire so the mobile UI's streaming
// code path works end-to-end. Once provider-side streaming lands, only
// this file changes — the mobile UI is already consuming token deltas.
//
// Format: standard SSE, one JSON object per `data:` line:
//
//	data: {"delta": "next chunk"}\n\n
//	data: {"delta": "another chunk"}\n\n
//	event: done\n
//	data: {"usage":{"prompt":12,"completion":34}}\n\n
//
// Or on error mid-stream:
//
//	event: error\n
//	data: {"error":"…"}\n\n
package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/zdaniels/fathom/pkg/types"
)

// chunkPause is the inter-chunk delay for the fake-stream path. Short
// enough to feel responsive, long enough that the typing effect is
// visible on mobile.
const chunkPause = 25 * time.Millisecond

// chunkSize is roughly one "word group" — pick what looks natural in
// the mobile UI. We chunk on word boundaries when possible.
const chunkSize = 12

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

	reply, err := handler(r.Context(), msg, sess)
	if err != nil {
		writeSSEEvent(w, "error", map[string]string{"error": err.Error()})
		flusher.Flush()
		return
	}

	// Chunk the reply and emit. Chunks split on whitespace when possible
	// to avoid breaking mid-word — looks cleaner in the mobile UI as
	// the bubble grows.
	chunks := chunkText(reply, chunkSize)
	for _, c := range chunks {
		// Bail if the client went away.
		select {
		case <-r.Context().Done():
			return
		default:
		}
		writeSSEEvent(w, "message", map[string]string{"delta": c})
		flusher.Flush()
		time.Sleep(chunkPause)
	}
	writeSSEEvent(w, "done", map[string]interface{}{
		"session_id": sess.ID,
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

// chunkText splits text into chunks of roughly `target` runes, breaking
// on whitespace when possible so words stay intact. The mobile UI's
// markdown renderer can handle partial markdown sequences across
// chunks (it re-renders from the full accumulated buffer on each delta).
func chunkText(text string, target int) []string {
	if len(text) <= target {
		return []string{text}
	}
	var out []string
	runes := []rune(text)
	i := 0
	for i < len(runes) {
		end := i + target
		if end >= len(runes) {
			out = append(out, string(runes[i:]))
			break
		}
		// Walk back to the last whitespace within the target window
		// so we don't split a word. Cap the walkback at half-target
		// to avoid producing tiny chunks when whitespace is sparse.
		split := end
		for split > i+target/2 && !isSpaceRune(runes[split]) {
			split--
		}
		if split <= i+target/2 {
			split = end // no good whitespace — split mid-word
		}
		out = append(out, string(runes[i:split]))
		i = split
	}
	return out
}

func isSpaceRune(r rune) bool {
	return r == ' ' || r == '\n' || r == '\t' || r == '\r'
}
