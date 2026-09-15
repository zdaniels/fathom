package gateway

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// EventBus is a tiny in-memory pub/sub for cross-component notifications
// the gateway wants to surface to clients (currently: the macOS app
// subscribing to scheduled-job completions). Bounded ring buffer +
// long-poll endpoint — no WebSockets, no SSE complexity, no per-client
// state to track.
//
// Why this and not /api/v1/audit polling: the audit log records EVERY
// tool call + LLM request, which is way more chatter than notifications
// want. Events are the small, user-facing subset.
type EventBus struct {
	mu        sync.Mutex
	events    []Event
	maxEvents int
	nextID    int64
	cond      *sync.Cond
}

// Event is one published occurrence. Type is what kind ("scheduled_job_done"),
// detail is arbitrary JSON the consumer interprets.
type Event struct {
	ID        int64                  `json:"id"`
	Timestamp time.Time              `json:"timestamp"`
	Type      string                 `json:"type"`
	Detail    map[string]interface{} `json:"detail"`
}

// NewEventBus returns a bus with a bounded ring buffer.
func NewEventBus(maxEvents int) *EventBus {
	if maxEvents <= 0 {
		maxEvents = 1000
	}
	bus := &EventBus{maxEvents: maxEvents}
	bus.cond = sync.NewCond(&bus.mu)
	return bus
}

// Publish records an event. Returns its assigned id. Wakes any long-poll
// readers waiting on new data.
func (b *EventBus) Publish(eventType string, detail map[string]interface{}) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := atomic.AddInt64(&b.nextID, 1)
	e := Event{
		ID:        id,
		Timestamp: time.Now().UTC(),
		Type:      eventType,
		Detail:    detail,
	}
	b.events = append(b.events, e)
	if len(b.events) > b.maxEvents {
		// FIFO evict — copy down to keep slices small.
		b.events = b.events[len(b.events)-b.maxEvents:]
	}
	b.cond.Broadcast()
	return id
}

// Since returns events with id > sinceID. Doesn't block.
func (b *EventBus) Since(sinceID int64) []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sinceLocked(sinceID)
}

// Wait blocks up to timeout waiting for events with id > sinceID. Returns
// immediately if there are already matching events.
func (b *EventBus) Wait(sinceID int64, timeout time.Duration) []Event {
	deadline := time.Now().Add(timeout)
	b.mu.Lock()
	defer b.mu.Unlock()
	for {
		out := b.sinceLocked(sinceID)
		if len(out) > 0 {
			return out
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		// sync.Cond doesn't support timeout natively. We launch a timer
		// that broadcasts wake on expiry, then loop and re-check.
		timer := time.AfterFunc(remaining, func() {
			b.mu.Lock()
			b.cond.Broadcast()
			b.mu.Unlock()
		})
		b.cond.Wait()
		timer.Stop()
		// Re-check on wake; the loop top will return if we have events.
	}
}

func (b *EventBus) sinceLocked(sinceID int64) []Event {
	out := make([]Event, 0)
	for _, e := range b.events {
		if e.ID > sinceID {
			out = append(out, e)
		}
	}
	return out
}

// handleEvents is the gateway HTTP handler for GET /api/v1/events. Query
// params:
//
//	since  — return events with id > since (default 0)
//	wait   — long-poll up to this many seconds (default 0 = no wait;
//	         max 30 to keep clients from holding sockets forever)
//
// Auth: same bearer-token as other gateway endpoints. Returns
// `{events: [...], lastId: N}` so the client can use lastId as the next
// `since`.
func (g *Gateway) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	authRes, ok := g.authenticate(w, r)
	if !ok {
		return
	}
	if !g.executionAllowed(w, authRes.UserID) {
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	if wait > 30 {
		wait = 30
	}
	if wait < 0 {
		wait = 0
	}
	var evs []Event
	if wait > 0 {
		evs = g.Events.Wait(since, time.Duration(wait)*time.Second)
	} else {
		evs = g.Events.Since(since)
	}
	var lastID int64
	for _, e := range evs {
		if e.ID > lastID {
			lastID = e.ID
		}
	}
	if lastID == 0 {
		lastID = since
	}
	body := map[string]interface{}{
		"events": evs,
		"lastId": lastID,
	}
	_ = json.NewEncoder(w).Encode(body) //nolint:errcheck
	w.Header().Set("Content-Type", "application/json")
}
