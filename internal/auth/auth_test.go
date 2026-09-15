package auth

import (
	"testing"
	"time"
)

func TestCreateAPITokenAndAuthenticate(t *testing.T) {
	m := New()
	token, err := m.CreateAPIToken("alice", "test")
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if token == "" {
		t.Fatal("CreateAPIToken returned empty token")
	}
	res, err := m.Authenticate(token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if res.UserID != "alice" {
		t.Errorf("UserID = %q, want alice", res.UserID)
	}
	if res.Method != "token" {
		t.Errorf("Method = %q, want token", res.Method)
	}
}

func TestAuthenticateRejectsBadToken(t *testing.T) {
	m := New()
	if _, err := m.Authenticate(""); err == nil {
		t.Error("empty token must error")
	}
	if _, err := m.Authenticate("definitely-not-a-real-token"); err == nil {
		t.Error("unknown token must error")
	}
}

func TestHasTokensReflectsState(t *testing.T) {
	m := New()
	if m.HasTokens() {
		t.Error("new manager should have no tokens")
	}
	_, _ = m.SetupInitialToken()
	if !m.HasTokens() {
		t.Error("HasTokens should be true after SetupInitialToken")
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := NewSessionStore(time.Hour)
	sess := s.Create("alice", "device1")
	if sess.ID == "" || sess.UserID != "alice" {
		t.Fatalf("Create returned bad session: %+v", sess)
	}

	got, ok := s.Get(sess.ID)
	if !ok || got.ID != sess.ID {
		t.Errorf("Get didn't return the session")
	}
	if s.Count() != 1 {
		t.Errorf("Count = %d, want 1", s.Count())
	}

	by := s.ByUser("alice")
	if len(by) != 1 {
		t.Errorf("ByUser len = %d, want 1", len(by))
	}

	s.Destroy(sess.ID)
	if _, ok := s.Get(sess.ID); ok {
		t.Error("Get after Destroy should miss")
	}
	if s.Count() != 0 {
		t.Errorf("Count after Destroy = %d, want 0", s.Count())
	}
}

func TestSessionCleanupRemovesExpired(t *testing.T) {
	// Tiny timeout so we don't have to wait.
	s := NewSessionStore(50 * time.Millisecond)
	sess := s.Create("alice", "device1")
	time.Sleep(100 * time.Millisecond)
	s.Cleanup()
	if _, ok := s.Get(sess.ID); ok {
		t.Error("expired session should be cleaned up")
	}
	if s.Count() != 0 {
		t.Errorf("Count after Cleanup = %d, want 0", s.Count())
	}
}
