package auth

import (
	"sync"
	"time"

	"github.com/zdaniels/fathom/internal/security"
	"github.com/zdaniels/fathom/pkg/types"
)

// SessionStore is the in-memory session manager. Sessions auto-expire based
// on the configured timeout; Cleanup is called periodically by the gateway.
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]types.Session
	timeout  time.Duration
}

// NewSessionStore returns a fresh store with the given TTL. The gateway
// invokes Cleanup on a tick to expire stale sessions.
func NewSessionStore(timeout time.Duration) *SessionStore {
	if timeout <= 0 {
		timeout = time.Hour
	}
	return &SessionStore{
		sessions: make(map[string]types.Session),
		timeout:  timeout,
	}
}

// Create issues a new session for userID. Permissions default to the most
// restrictive setup; gateway / agent path can refine.
func (s *SessionStore) Create(userID, deviceID string) types.Session {
	now := time.Now().UTC()
	sess := types.Session{
		ID:        security.GenerateID(),
		UserID:    userID,
		DeviceID:  deviceID,
		CreatedAt: now,
		ExpiresAt: now.Add(s.timeout),
		Permissions: types.PermissionSet{
			Network:    "allow",
			Filesystem: "read-write",
			Shell:      "deny",
			Secrets:    "accessible",
		},
	}
	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.mu.Unlock()
	return sess
}

// Get returns the session for id (or false if missing/expired).
func (s *SessionStore) Get(id string) (types.Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[id]
	if !ok || time.Now().After(sess.ExpiresAt) {
		return types.Session{}, false
	}
	return sess, true
}

// Refresh slides the session's expiry forward by the configured TTL. Called
// by the gateway after every successful request.
func (s *SessionStore) Refresh(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return
	}
	sess.ExpiresAt = time.Now().Add(s.timeout)
	s.sessions[id] = sess
}

// Destroy removes a session immediately.
func (s *SessionStore) Destroy(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

// ByUser returns all live sessions belonging to userID.
func (s *SessionStore) ByUser(userID string) []types.Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	var out []types.Session
	for _, sess := range s.sessions {
		if sess.UserID == userID && now.Before(sess.ExpiresAt) {
			out = append(out, sess)
		}
	}
	return out
}

// Cleanup removes expired sessions. Idempotent.
func (s *SessionStore) Cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, sess := range s.sessions {
		if now.After(sess.ExpiresAt) {
			delete(s.sessions, id)
		}
	}
}

// Count returns the live session count.
func (s *SessionStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}
