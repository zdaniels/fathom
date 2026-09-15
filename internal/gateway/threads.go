// Gateway-side wiring for persistent threads.
//
// Two distinct concerns here, deliberately in one file because they
// share types and the file stays small:
//
//   - ThreadHub: per-thread pub-sub. Subscribers register a buffered
//     channel; publishers fan out to all live subscribers. Used by SSE
//     to push user_message / agent_delta / agent_done events to every
//     device currently watching a thread.
//
//   - HTTP handlers for /api/v1/threads/{*}. Mostly thin wrappers
//     around internal/threads.Store, plus the POST messages handler
//     that calls the agent and publishes back.
//
// Why a hub rather than reusing internal/gateway.EventBus: EventBus is
// global, long-poll, append-only. Threads need topic-routing + drop-
// on-overflow semantics so a slow phone reader can't backpressure the
// agent loop.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zdaniels/fathom/internal/agent"
	"github.com/zdaniels/fathom/internal/agentfactory"
	"github.com/zdaniels/fathom/internal/auth"
	"github.com/zdaniels/fathom/internal/threads"
	"github.com/zdaniels/fathom/pkg/types"
)

// ThreadEvent is one notification on a thread topic. Type drives how
// clients render it.
type ThreadEvent struct {
	Type     string                 `json:"type"` // "user_message" | "agent_delta" | "agent_done" | "agent_error"
	ThreadID string                 `json:"thread_id"`
	Message  *threads.Message       `json:"message,omitempty"`
	Delta    string                 `json:"delta,omitempty"`
	Usage    map[string]interface{} `json:"usage,omitempty"`
	Error    string                 `json:"error,omitempty"`
}

// swarmEventLabel turns a swarm progress event into a short human-readable
// line for the agent_swarm thread event clients render.
func swarmEventLabel(ev agent.SwarmEvent) string {
	switch ev.Kind {
	case "round_start":
		return "swarm: " + ev.Detail
	case "post":
		return fmt.Sprintf("swarm: %s → %s", ev.Author, ev.Channel)
	case "synthesis":
		return "swarm: synthesising"
	case "complete":
		return "swarm: done (" + ev.Detail + ")"
	default:
		return "swarm: " + ev.Kind
	}
}

// ThreadHub fans events out to per-thread subscribers. Construct one
// per process; methods are safe for concurrent use.
type ThreadHub struct {
	mu   sync.RWMutex
	subs map[string]map[chan ThreadEvent]struct{} // threadID → set of channels
}

func NewThreadHub() *ThreadHub {
	return &ThreadHub{subs: map[string]map[chan ThreadEvent]struct{}{}}
}

// Subscribe registers for events on threadID. Returns the receive
// channel and an unsubscribe func; caller MUST call unsubscribe on
// shutdown (else the hub leaks).
//
// The channel is buffered; if it fills (slow reader), the publisher
// drops the event for that subscriber rather than blocking — the
// alternative is to let one stuck client stall every other.
func (h *ThreadHub) Subscribe(threadID string) (<-chan ThreadEvent, func()) {
	ch := make(chan ThreadEvent, 32)
	h.mu.Lock()
	if h.subs[threadID] == nil {
		h.subs[threadID] = map[chan ThreadEvent]struct{}{}
	}
	h.subs[threadID][ch] = struct{}{}
	h.mu.Unlock()
	unsubscribe := func() {
		h.mu.Lock()
		if set, ok := h.subs[threadID]; ok {
			delete(set, ch)
			if len(set) == 0 {
				delete(h.subs, threadID)
			}
		}
		h.mu.Unlock()
		close(ch)
	}
	return ch, unsubscribe
}

// Publish fans out an event to every current subscriber of evt.ThreadID.
// Non-blocking — full channels drop the event for that subscriber.
func (h *ThreadHub) Publish(evt ThreadEvent) {
	h.mu.RLock()
	subs := h.subs[evt.ThreadID]
	// Snapshot the channel set under the read lock so we can release
	// before the (possibly slow) sends.
	chans := make([]chan ThreadEvent, 0, len(subs))
	for c := range subs {
		chans = append(chans, c)
	}
	h.mu.RUnlock()
	delivered := 0
	dropped := 0
	for _, c := range chans {
		select {
		case c <- evt:
			delivered++
		default:
			dropped++
		}
	}
	slog.Debug("threadhub publish",
		"type", evt.Type, "thread", evt.ThreadID,
		"subscribers", len(chans), "delivered", delivered, "dropped", dropped)
}

// === HTTP handlers ==========================================================

// handleThreadsRoot routes /api/v1/threads (no trailing slash) — GET
// for list, POST for create. The trailing-slash variant is owned by
// handleThreadItem.
func (g *Gateway) handleThreadsRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		g.handleThreadsList(w, r)
	case http.MethodPost:
		g.handleThreadsCreate(w, r)
	default:
		jsonError(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

// handleThreadsList: GET /api/v1/threads → list of the caller's threads.
// Newest first, capped at 50.
func (g *Gateway) handleThreadsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	res, ok := g.authenticate(w, r)
	if !ok {
		return
	}
	if g.Threads == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"threads": []interface{}{}})
		return
	}
	list, err := g.Threads.List(res.UserID, 50)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Go marshals nil slices as `null`; Kotlin's kotlinx.serialization
	// is strict and rejects null where a non-nullable array is
	// expected, breaking the mobile clients ("expected start of the
	// array '{' but had n instead"). Force an empty slice so the
	// wire shape is always `"threads": []`.
	writeJSON(w, http.StatusOK, map[string]interface{}{"threads": ensureThreads(list)})
}

// ensureThreads is the list-of-threads twin of ensureMessages — same
// reason, same fix.
func ensureThreads(t []threads.Thread) []threads.Thread {
	if t == nil {
		return []threads.Thread{}
	}
	return t
}

// handleThreadsCreate: POST /api/v1/threads {title?} → new thread.
func (g *Gateway) handleThreadsCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	res, ok := g.authenticate(w, r)
	if !ok {
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 2048))
	var in struct {
		Title string `json:"title"`
	}
	if len(body) > 0 {
		_ = json.Unmarshal(body, &in)
	}
	if g.Threads == nil {
		jsonError(w, http.StatusServiceUnavailable, "thread store not configured")
		return
	}
	t, err := g.Threads.Create(res.UserID, strings.TrimSpace(in.Title))
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// handleThreadItem routes anything under /api/v1/threads/{id...}:
//
//	GET    /api/v1/threads/{id}                  → metadata + tail messages
//	GET    /api/v1/threads/{id}/messages?after=  → after a timestamp
//	GET    /api/v1/threads/{id}/messages?before= → before a timestamp (scrollback)
//	GET    /api/v1/threads/{id}/stream           → SSE pub-sub
//	POST   /api/v1/threads/{id}/messages         → user msg → agent → stream back
//	PATCH  /api/v1/threads/{id}                  → rename
//	DELETE /api/v1/threads/{id}                  → soft-delete
//
// Special: when the {id} segment is the literal string "search", the
// request routes to the FTS5-backed message search instead — that's
// the GET /api/v1/threads/search?q=... endpoint.
func (g *Gateway) handleThreadItem(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/threads/")
	parts := strings.SplitN(path, "/", 2)
	threadID := parts[0]
	if threadID == "" {
		jsonError(w, http.StatusBadRequest, "missing thread id")
		return
	}
	// Reserved-id route: /api/v1/threads/search bypasses thread lookup.
	if threadID == "search" {
		g.serveSearchMessages(w, r)
		return
	}
	rest := ""
	if len(parts) == 2 {
		rest = parts[1]
	}
	res, ok := g.authenticate(w, r)
	if !ok {
		return
	}
	if g.Threads == nil {
		jsonError(w, http.StatusServiceUnavailable, "thread store not configured")
		return
	}

	// Authorise: caller must own the thread (single-user mode for now;
	// when team mode lands, swap in a tenant check).
	t, err := g.Threads.Get(threadID)
	switch {
	case errors.Is(err, threads.ErrNotFound):
		jsonError(w, http.StatusNotFound, "no such thread")
		return
	case err != nil:
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if t.UserID != res.UserID {
		jsonError(w, http.StatusForbidden, "not your thread")
		return
	}

	switch {
	case rest == "" && r.Method == http.MethodGet:
		g.serveThreadMetadata(w, r, t)
	case rest == "" && r.Method == http.MethodPatch:
		g.serveThreadRename(w, r, t)
	case rest == "" && r.Method == http.MethodDelete:
		g.serveThreadDelete(w, t)
	case rest == "messages" && r.Method == http.MethodGet:
		g.serveThreadMessages(w, r, t)
	case rest == "messages" && r.Method == http.MethodPost:
		g.serveThreadSendMessage(w, r, t, res)
	case rest == "stream" && r.Method == http.MethodGet:
		g.serveThreadStream(w, r, t)
	case rest == "restore" && r.Method == http.MethodPost:
		g.serveThreadRestore(w, t)
	case rest == "usage" && r.Method == http.MethodGet:
		g.serveThreadUsage(w, t)
	default:
		jsonError(w, http.StatusNotFound, "no such route")
	}
}

func (g *Gateway) serveThreadRestore(w http.ResponseWriter, t threads.Thread) {
	if err := g.Threads.Restore(t.ID); err != nil {
		if errors.Is(err, threads.ErrNotFound) {
			jsonError(w, http.StatusNotFound, "no deleted thread by that id")
			return
		}
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"restored": t.ID})
}

// --- per-thread handlers ----------------------------------------------------

func (g *Gateway) serveThreadMetadata(w http.ResponseWriter, _ *http.Request, t threads.Thread) {
	msgs, err := g.Threads.MessagesTail(t.ID, 50)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Same nil-slice gotcha as the other endpoints — the mobile parser
	// rejects {"messages": null}. See ensureMessages for the full story.
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"thread":   t,
		"messages": ensureMessages(msgs),
	})
}

func (g *Gateway) serveThreadRename(w http.ResponseWriter, r *http.Request, t threads.Thread) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 2048))
	var in struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		jsonError(w, http.StatusBadRequest, "bad json")
		return
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		jsonError(w, http.StatusBadRequest, "title required")
		return
	}
	if err := g.Threads.Rename(t.ID, title); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"id": t.ID, "title": title})
}

func (g *Gateway) serveThreadDelete(w http.ResponseWriter, t threads.Thread) {
	if err := g.Threads.SoftDelete(t.ID); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": t.ID})
}

func (g *Gateway) serveThreadMessages(w http.ResponseWriter, r *http.Request, t threads.Thread) {
	q := r.URL.Query()
	limit := parseLimit(q.Get("limit"), 50)
	if after := q.Get("after"); after != "" {
		ts, err := time.Parse(time.RFC3339Nano, after)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "after must be RFC3339")
			return
		}
		msgs, err := g.Threads.MessagesAfter(t.ID, ts, limit)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"messages": ensureMessages(msgs)})
		return
	}
	if before := q.Get("before"); before != "" {
		ts, err := time.Parse(time.RFC3339Nano, before)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "before must be RFC3339")
			return
		}
		msgs, err := g.Threads.MessagesBefore(t.ID, ts, limit)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"messages": ensureMessages(msgs)})
		return
	}
	// No bookmark — return the tail.
	msgs, err := g.Threads.MessagesTail(t.ID, limit)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"messages": ensureMessages(msgs)})
}

// ensureMessages collapses a nil slice to an empty slice so the JSON
// shape is always `[]` rather than `null`. Mobile clients (Kotlin
// kotlinx.serialization, Swift Decodable when the field is not
// `[Foo]?`) refuse to parse null where a non-nullable list is
// declared. See ensureThreads for the matching list-of-threads variant.
func ensureMessages(m []threads.Message) []threads.Message {
	if m == nil {
		return []threads.Message{}
	}
	return m
}

// serveThreadSendMessage: the heavy one. Persists the user message,
// publishes to the hub, invokes the agent, persists + publishes the
// agent reply. Returns the agent reply in the HTTP response (so simple
// non-SSE clients still get a synchronous answer) AND fans events out
// to every device subscribed via /stream.
func (g *Gateway) serveThreadSendMessage(w http.ResponseWriter, r *http.Request, t threads.Thread, authRes auth.Result) {
	if !g.executionAllowed(w, authRes.UserID) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "body unreadable")
		return
	}
	var in struct {
		Text            string `json:"text"`
		ClientMessageID string `json:"clientMessageId"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.Text == "" {
		jsonError(w, http.StatusBadRequest, "missing 'text' field")
		return
	}

	if len(in.ClientMessageID) > 128 {
		jsonError(w, 400, "clientMessageId is too long")
		return
	}
	userMetadata := map[string]interface{}{}
	if in.ClientMessageID != "" {
		userMetadata["clientMessageId"] = in.ClientMessageID
	}

	// Slash-command interception. `/model`, `/models`, `/model <name>`,
	// `/model default` are handled entirely by the gateway and never
	// reach the agent. The exchange is still persisted to the thread
	// so cross-device clients see the command history (the user typed
	// it from one device and the others' UIs should reflect the new
	// state). Output appears as a synthetic agent_message.
	if cmdReply, handled := g.maybeHandleSlashCommand(t, in.Text, authRes); handled {
		g.writeCannedReply(w, t, authRes, in.Text, cmdReply, userMetadata)
		return
	}
	if idReply, handled := maybeHandleIdentityQuestion(in.Text); handled {
		g.writeCannedReply(w, t, authRes, in.Text, idReply, userMetadata)
		return
	}
	if modelReply, handled := g.maybeHandleModelQuestion(t, in.Text); handled {
		g.writeCannedReply(w, t, authRes, in.Text, modelReply, userMetadata)
		return
	}

	// Persist + publish the user message.
	userMsg, err := g.Threads.Append(t.ID, "user", in.Text, authRes.DeviceID, userMetadata)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if g.ThreadHub != nil {
		g.ThreadHub.Publish(ThreadEvent{
			Type: "user_message", ThreadID: t.ID, Message: &userMsg,
		})
		// Signal every subscribed device that the agent is now working
		// on this thread. Each client uses this to show the spinner
		// even when it wasn't the one that hit send.
		g.ThreadHub.Publish(ThreadEvent{
			Type: "agent_thinking", ThreadID: t.ID, Message: &userMsg,
		})
	}

	g.mu.RLock()
	handler := g.msgHandler
	handlerN := g.msgHandlerN
	handlerNU := g.msgHandlerNU
	g.mu.RUnlock()
	if handler == nil && handlerN == nil && handlerNU == nil {
		jsonError(w, http.StatusServiceUnavailable, "no message handler configured")
		return
	}

	// Build a session for the agent call. The session is short-lived
	// (one POST), distinct from the thread which is persistent.
	sess := g.Sessions.Create(authRes.UserID, remoteAddr(r))
	defer g.Sessions.Destroy(sess.ID)
	msg := types.ChannelMessage{
		ChannelType: "thread",
		ChannelID:   t.ID,
		SenderID:    authRes.UserID,
		Text:        in.Text,
		Timestamp:   time.Now().UTC(),
	}
	// Surface sub-agent delegation to subscribed devices: the delegate
	// tool calls this notifier (carried via context) just before a child
	// loop starts, so clients can show "→ delegating to <model>". Clients
	// that don't render the event ignore it.
	dctx := r.Context()
	if g.ThreadHub != nil {
		dctx = agent.WithDelegateNotifier(dctx, func(model, taskPreview string) {
			label := "delegating"
			if model != "" {
				label = "delegating to " + model
			}
			g.ThreadHub.Publish(ThreadEvent{
				Type: "agent_delegating", ThreadID: t.ID, Delta: label,
			})
		})
		// Swarm progress: stream round/post/synthesis events so clients can
		// watch the blackboard fill in, the same way delegation is surfaced.
		dctx = agent.WithSwarmObserver(dctx, func(ev agent.SwarmEvent) {
			g.ThreadHub.Publish(ThreadEvent{
				Type: "agent_swarm", ThreadID: t.ID, Delta: swarmEventLabel(ev),
			})
		})
	}

	// Dispatch preference: HandlerNU > HandlerN > Handler. HandlerNU
	// returns usage data we persist onto the agent message; the others
	// don't. The per-thread model override is honored at every tier.
	var reply string
	var agentUsage *UsageSnapshot
	var resolvedModel string
	switch {
	case handlerNU != nil:
		reply, agentUsage, resolvedModel, err = handlerNU(dctx, msg, sess, t.Model)
	case t.Model != "" && handlerN != nil:
		reply, err = handlerN(dctx, msg, sess, t.Model)
	case handler != nil:
		reply, err = handler(dctx, msg, sess)
	case handlerN != nil:
		// No regular handler but we have HandlerN — call it with the
		// empty string to let the router pick its default.
		reply, err = handlerN(dctx, msg, sess, "")
	}
	if err != nil {
		if g.ThreadHub != nil {
			g.ThreadHub.Publish(ThreadEvent{
				Type: "agent_error", ThreadID: t.ID, Error: err.Error(),
			})
		}
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Build the agent message's metadata. Only persist non-zero
	// usage + a non-empty model name so messages from local providers
	// (Ollama) that don't report tokens don't carry useless zero fields.
	var agentMeta map[string]interface{}
	if agentUsage != nil || resolvedModel != "" {
		agentMeta = map[string]interface{}{}
		if agentUsage != nil {
			agentMeta["usage"] = agentUsage
		}
		if resolvedModel != "" {
			agentMeta["model"] = resolvedModel
		}
	}
	agentMsg, err := g.Threads.Append(t.ID, "agent", reply, "", agentMeta)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if g.ThreadHub != nil {
		g.ThreadHub.Publish(ThreadEvent{
			Type: "agent_done", ThreadID: t.ID, Message: &agentMsg,
		})
	}

	// Auto-title if the thread didn't have one yet. Pure-text strategy —
	// no extra LLM call — but smarter than a naive truncate:
	//   - strip code fences (```… ```) so the title isn't a snippet
	//   - take only the first sentence (split on . ? !)
	//   - trim to 50 chars
	// LLM-summarize titles are a future option; the text path covers the
	// common case (an actual question or directive) cleanly and for free.
	if t.Title == "" {
		title := autoTitle(in.Text)
		_ = g.Threads.Rename(t.ID, title)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"user_message":  userMsg,
		"agent_message": agentMsg,
		"session_id":    sess.ID,
	})
}

// serveThreadStream: SSE subscription. Streams every ThreadEvent the
// hub publishes for this thread until the client disconnects. Sends a
// `: ping` heartbeat every 25s to keep proxies / iOS Safari from
// dropping the connection.
func (g *Gateway) serveThreadStream(w http.ResponseWriter, r *http.Request, t threads.Thread) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, http.StatusInternalServerError, "streaming not supported by transport")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if g.ThreadHub == nil {
		writeSSEEvent(w, "error", map[string]string{"error": "no thread hub"})
		flusher.Flush()
		return
	}
	ch, unsubscribe := g.ThreadHub.Subscribe(t.ID)
	defer unsubscribe()
	subscribedAt := time.Now()
	slog.Info("SSE subscribe", "thread", t.ID, "remote", remoteAddr(r))
	defer func() {
		slog.Info("SSE unsubscribe", "thread", t.ID,
			"remote", remoteAddr(r),
			"duration", time.Since(subscribedAt).Round(time.Second))
	}()

	// Initial hello so the client knows the stream is live.
	writeSSEEvent(w, "ready", map[string]interface{}{
		"thread_id":   t.ID,
		"server_time": time.Now().UTC(),
	})
	flusher.Flush()

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	eventCount := 0
	for {
		select {
		case <-r.Context().Done():
			slog.Info("SSE client disconnected", "thread", t.ID, "events_sent", eventCount)
			return
		case <-heartbeat.C:
			_, _ = fmt.Fprintln(w, ": ping")
			flusher.Flush()
		case evt, open := <-ch:
			if !open {
				slog.Info("SSE channel closed", "thread", t.ID, "events_sent", eventCount)
				return
			}
			writeSSEEvent(w, evt.Type, evt)
			flusher.Flush()
			eventCount++
		}
	}
}

// autoTitle generates a thread title from the first user message
// without invoking the agent. Algorithm:
//
//  1. Remove fenced code blocks — they're never what the user "meant"
//     as a title.
//  2. Collapse whitespace to single spaces.
//  3. Take the first sentence (split on . ? ! followed by space or end).
//  4. Truncate to 50 chars with ellipsis.
//
// Empty input → "New chat". 80% as good as LLM-summarized titles for
// 0% of the cost.
func autoTitle(text string) string {
	text = stripFences(text)
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return "New chat"
	}
	// First sentence — scan for ., ?, ! followed by space or EOS.
	// Question marks and exclamation marks stay in the title because
	// they're load-bearing (a title without "?" reads as a statement,
	// not a question). Plain periods drop.
	end := len(text)
	for i, r := range text {
		if r == '.' || r == '?' || r == '!' {
			if i+1 == len(text) || text[i+1] == ' ' {
				if r == '.' {
					end = i // drop the period
				} else {
					end = i + 1 // keep ? and !
				}
				break
			}
		}
	}
	first := strings.TrimSpace(text[:end])
	const max = 60
	if len(first) > max {
		first = strings.TrimSpace(first[:max-1]) + "…"
	}
	if first == "" {
		return "New chat"
	}
	return first
}

// stripFences removes ```…``` (and indented) code blocks so they
// can't pollute auto-generated titles.
func stripFences(text string) string {
	const fence = "```"
	for {
		i := strings.Index(text, fence)
		if i < 0 {
			break
		}
		j := strings.Index(text[i+len(fence):], fence)
		if j < 0 {
			// Unterminated fence — drop everything from it onward.
			text = strings.TrimSpace(text[:i])
			break
		}
		text = text[:i] + text[i+len(fence)+j+len(fence):]
	}
	return strings.TrimSpace(text)
}

// parseLimit clamps a string limit to [1, 200]. Default `dflt` when
// missing or unparseable.
func parseLimit(s string, dflt int) int {
	if s == "" {
		return dflt
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return dflt
	}
	if n < 1 {
		return 1
	}
	if n > 200 {
		return 200
	}
	return n
}

// fanOutContextDoneEnsuringTypeImport keeps the imports honest in case
// of build-flag combinations that elide the only context use above.
var _ = context.Background

// serveSearchMessages: GET /api/v1/threads/search?q=...&limit=...
//
// Runs an FTS5 full-text query against this user's messages and
// returns matching rows in relevance order. Empty `q` returns an
// empty array (not an error) — keeps clients that bind a search box
// to a live query from spamming 400s on every backspace.
func (g *Gateway) serveSearchMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	res, ok := g.authenticate(w, r)
	if !ok {
		return
	}
	if g.Threads == nil {
		jsonError(w, http.StatusServiceUnavailable, "thread store not configured")
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := parseLimit(r.URL.Query().Get("limit"), 50)

	if q == "" {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"query": "", "hits": []interface{}{},
		})
		return
	}

	hits, err := g.Threads.SearchMessages(res.UserID, q, limit)
	if err != nil {
		// Most FTS5 errors are user-supplied query-syntax issues
		// (unmatched quotes, bare operators, etc.). Return them as
		// 400 so the client can show "search syntax invalid" instead
		// of treating it as a server error.
		jsonError(w, http.StatusBadRequest, "search: "+err.Error())
		return
	}
	if hits == nil {
		hits = []threads.SearchHit{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"query": q, "hits": hits,
	})
}

// writeCannedReply persists + publishes both the user message and a
// gateway-synthesised agent reply, then returns the HTTP response in
// the same shape as the normal agent path. Shared between slash
// commands and the identity-question intercept — both bypass the LLM
// and need exactly the same plumbing.
func (g *Gateway) writeCannedReply(w http.ResponseWriter, t threads.Thread, authRes auth.Result, userText, agentText string, userMetadata map[string]interface{}) {
	userMsg, err := g.Threads.Append(t.ID, "user", userText, authRes.DeviceID, userMetadata)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	agentMsg, err := g.Threads.Append(t.ID, "agent", agentText, "", nil)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if g.ThreadHub != nil {
		g.ThreadHub.Publish(ThreadEvent{
			Type: "user_message", ThreadID: t.ID, Message: &userMsg,
		})
		g.ThreadHub.Publish(ThreadEvent{
			Type: "agent_done", ThreadID: t.ID, Message: &agentMsg,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"user_message":  userMsg,
		"agent_message": agentMsg,
	})
}

// identityPhrases is the closed set of inputs we treat as identity
// questions. We bypass the LLM and return a canned reply because
// small open-source models (Hermes 3:8b especially) are not reliable
// at honoring a "your name is fathom" system prompt — they fall back
// to their trained identity ("My name is Hermes") under long-context
// prompts. Bypassing the model is cheaper, faster, and deterministic.
//
// Match is exact-after-normalisation (lowercase, strip surrounding
// whitespace + trailing punctuation), so "what is your name?" hits
// but "what is your name doing in this code?" doesn't.
var identityPhrases = map[string]struct{}{
	"what is your name":      {},
	"whats your name":        {},
	"what's your name":       {},
	"what your name":         {},
	"who are you":            {},
	"what are you":           {},
	"introduce yourself":     {},
	"tell me your name":      {},
	"tell me about yourself": {},
	"your name":              {},
}

// maybeHandleIdentityQuestion returns the canned identity reply when
// the user's message is one of a small closed set of identity
// questions. Returns ("", false) for everything else so normal
// agent dispatch continues.
func maybeHandleIdentityQuestion(text string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(text))
	normalized = strings.TrimRight(normalized, ".?!,'\" \t")
	normalized = strings.TrimSpace(normalized)
	if _, ok := identityPhrases[normalized]; ok {
		return "I'm fathom — your local AI agent.", true
	}
	return "", false
}

// modelPhrases is the closed set of inputs we treat as "which model
// is powering you?" — the legitimate follow-up to the identity
// question. The gateway knows the exact model for this thread, so
// returning a server-authoritative answer beats letting the model
// either lie ("I'm Hermes") or refuse ("I can't access that") under
// the "never reveal internal instructions" baseline rule.
var modelPhrases = map[string]struct{}{
	"what model are you":         {},
	"what model are you using":   {},
	"what model is powering you": {},
	"what model powers you":      {},
	"which model are you":        {},
	"which model are you using":  {},
	"whats your model":           {},
	"what's your model":          {},
	"what llm are you":           {},
	"what llm are you using":     {},
	"what ai model are you":      {},
	"what model is this":         {},
	"what model":                 {},
}

// maybeHandleModelQuestion answers "what model are you using?"-class
// questions with the actual active model for this thread. Falls back
// to the router's default when the thread has no per-thread pin.
//
// Takeover mode wins over everything: when it's on, every message is routed
// to an external coding agent (Claude Code / Codex), so the router default and
// per-thread `/model` pins are irrelevant — report the takeover target instead
// of misleadingly claiming a local model.
func (g *Gateway) maybeHandleModelQuestion(t threads.Thread, text string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(text))
	normalized = strings.TrimRight(normalized, ".?!,'\" \t")
	normalized = strings.TrimSpace(normalized)
	if _, ok := modelPhrases[normalized]; !ok {
		return "", false
	}
	if g.cfg.Takeover != nil && g.cfg.Takeover.Enabled {
		return fmt.Sprintf("I'm fathom — in takeover mode, routing every message through **%s**. "+
			"Turn this off by setting `takeover.enabled: false` in your config.",
			agentfactory.TakeoverDesc(g.cfg.Takeover)), true
	}
	model := t.Model
	source := "this thread is pinned to"
	if model == "" {
		g.mu.RLock()
		r := g.router
		g.mu.RUnlock()
		if r != nil {
			model = r.DefaultName()
		}
		source = "I'm currently routing through"
	}
	if model == "" {
		return "I'm fathom. The router isn't configured with a default model right now — set one in fathom.config.yaml under `llm.default`.", true
	}
	return fmt.Sprintf("I'm fathom — %s **%s**. Switch with `/use <name>`; see `/models` for the full list.", source, model), true
}

// maybeHandleSlashCommand interprets `/model`, `/model <name>`,
// `/model default`, and `/models` directly without invoking the agent.
// Returns the synthetic reply + true when handled; ("", false) for any
// other input (including normal text and unknown slash commands —
// unknown slashes fall through to the agent so prompts like
// "/etc/hosts shows..." aren't accidentally swallowed).
//
// Side effect: `/model <name>` persists the choice on the thread row
// (Threads.SetModel) so it sticks across reconnects + cross-device.
func (g *Gateway) maybeHandleSlashCommand(t threads.Thread, text string, _ auth.Result) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "/") {
		return "", false
	}
	// Tokenize: first word = command, rest = args.
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return "", false
	}
	cmd := strings.ToLower(fields[0])
	switch cmd {
	case "/model", "/use":
		// /model        → show current + list
		// /model <name> → pin to <name>
		// /model default → clear pin
		if len(fields) == 1 {
			return g.formatModelsList(t.Model), true
		}
		name := fields[1]
		if strings.EqualFold(name, "default") {
			if err := g.Threads.SetModel(t.ID, ""); err != nil {
				return "Could not clear model: " + err.Error(), true
			}
			defaultName := "(router default)"
			g.mu.RLock()
			r := g.router
			g.mu.RUnlock()
			if r != nil {
				if dn := r.DefaultName(); dn != "" {
					defaultName = dn
				}
			}
			return "Reverted to default model: " + defaultName, true
		}
		// Validate against the registry when we have one.
		g.mu.RLock()
		r := g.router
		g.mu.RUnlock()
		if r != nil {
			known := false
			for _, n := range r.Names() {
				if n == name {
					known = true
					break
				}
			}
			if !known {
				return "Unknown model: " + name + "\n\n" + g.formatModelsList(t.Model), true
			}
		}
		if err := g.Threads.SetModel(t.ID, name); err != nil {
			return "Could not set model: " + err.Error(), true
		}
		return "Switched to model: " + name, true
	case "/models":
		return g.formatModelsList(t.Model), true
	case "/cost", "/usage":
		// Cumulative token spend for this thread. We don't convert to
		// $$$ — pricing varies per model and changes over time; the
		// raw token count is the honest number. UIs can multiply.
		return g.formatThreadUsage(t.ID), true
	case "/rename":
		// /rename        → show current title
		// /rename <text> → set new title
		// Lifted from the CLI so phone/web clients can rename too. The
		// CLI continues to handle /rename locally before the gateway
		// path; this case only fires for non-CLI senders.
		if len(fields) == 1 {
			current := strings.TrimSpace(t.Title)
			if current == "" {
				return "This thread has no title yet. Set one with `/rename <title>`.", true
			}
			return "Current title: **" + current + "**", true
		}
		title := strings.TrimSpace(strings.TrimPrefix(trimmed, fields[0]))
		if title == "" {
			return "Usage: `/rename <new title>`", true
		}
		if err := g.Threads.Rename(t.ID, title); err != nil {
			return "Could not rename: " + err.Error(), true
		}
		return "Renamed thread to **" + title + "**", true
	case "/help":
		return g.formatSlashHelp(), true
	}
	// Unknown slash — let the agent see it.
	return "", false
}

// formatSlashHelp lists the slash commands the gateway intercepts.
// CLI-only commands (/quit, /review) aren't listed here — they wouldn't
// do anything from a phone or web client anyway.
func (g *Gateway) formatSlashHelp() string {
	return "**Slash commands**\n\n" +
		"- `/model` — show the active model + the list\n" +
		"- `/model <name>` — switch this thread to a model\n" +
		"- `/model default` — clear the per-thread pin\n" +
		"- `/models` — same as `/model`\n" +
		"- `/cost` or `/usage` — total tokens used in this thread\n" +
		"- `/rename` — show the current title\n" +
		"- `/rename <title>` — rename this thread\n" +
		"- `/help` — this list"
}

func (g *Gateway) formatThreadUsage(threadID string) string {
	if g.Threads == nil {
		return "Thread storage isn't configured on this gateway."
	}
	u, err := g.Threads.SumThreadUsage(threadID)
	if err != nil {
		return "Could not compute usage: " + err.Error()
	}
	if u.MessageCount == 0 {
		return "No usage recorded for this thread yet. (Local providers like Ollama don't report tokens; cloud providers do.)"
	}
	suffix := ""
	if u.MessageCount != 1 {
		suffix = "s"
	}
	return fmt.Sprintf(
		"**Thread usage**\n\n"+
			"- Prompt tokens:     **%d**\n"+
			"- Completion tokens: **%d**\n"+
			"- Total:             **%d**\n"+
			"- Across %d agent message%s with usage data.",
		u.PromptTokens, u.CompletionTokens, u.TotalTokens,
		u.MessageCount, suffix)
}

func (g *Gateway) serveThreadUsage(w http.ResponseWriter, t threads.Thread) {
	usage, err := g.Threads.SumThreadUsage(t.ID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

func (g *Gateway) formatModelsList(current string) string {
	g.mu.RLock()
	r := g.router
	g.mu.RUnlock()
	if r == nil {
		return "Model switching is not configured on this gateway. Add an `llm.models` map to fathom.config.yaml and restart."
	}
	names := r.Names()
	if len(names) == 0 {
		return "No models registered (all entries missing API keys?). See `llm.models` in fathom.config.yaml."
	}
	defaultName := r.DefaultName()
	var b strings.Builder
	b.WriteString("**Available models** — switch with `/model <name>`\n\n")
	for _, n := range names {
		prefix := "- "
		if n == current {
			prefix = "- **[current]** "
		} else if current == "" && n == defaultName {
			prefix = "- **[default]** "
		}
		b.WriteString(prefix)
		b.WriteString(n)
		b.WriteByte('\n')
	}
	b.WriteString("\nUse `/model default` to clear a per-thread pin and revert to the router default.")
	return b.String()
}
