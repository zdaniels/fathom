package collab

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Subscribers receive coalesced invalidations, never private task data. Clients
// fetch the authorized snapshot after subscribing and after every notification.
func (s *Service) notifyLocked(workspace string) {
	for ch := range s.subscribers[workspace] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
func (s *Service) stream(w http.ResponseWriter, r *http.Request, workspace, user string) {
	s.mu.Lock()
	count := 0
	for _, clients := range s.subscribers {
		count += len(clients)
	}
	if s.closed || s.streamsStopped || count >= 128 {
		s.mu.Unlock()
		fail(w, 503, errors.New("live updates unavailable; retry shortly"))
		return
	}
	if s.subscribers == nil {
		s.subscribers = map[string]map[chan struct{}]bool{}
	}
	if s.subscribers[workspace] == nil {
		s.subscribers[workspace] = map[chan struct{}]bool{}
	}
	updates := make(chan struct{}, 1)
	s.subscribers[workspace][updates] = true
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.subscribers[workspace], updates)
		if len(s.subscribers[workspace]) == 0 {
			delete(s.subscribers, workspace)
		}
		s.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	send := func(message string) bool {
		// Recheck both the token and membership before every event/heartbeat.
		current, err := s.Auth(r)
		if err != nil || current != user || s.Store.Role(workspace, user) == "" {
			return false
		}
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err = fmt.Fprint(w, message); err != nil {
			return false
		}
		return controller.Flush() == nil
	}
	if !send("event: changed\ndata: {}\n\n") {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case _, ok := <-updates:
			if !ok || !send("event: changed\ndata: {}\n\n") {
				return
			}
		case <-heartbeat.C:
			if !send(": heartbeat\n\n") {
				return
			}
		}
	}
}

// StopStreams lets HTTP shutdown drain immediately while the store remains open
// for in-flight writes and agent outcome persistence.
func (s *Service) StopStreams() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamsStopped = true
	for _, clients := range s.subscribers {
		for ch := range clients {
			close(ch)
		}
	}
	s.subscribers = nil
}
