package relay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveAndLoadConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.token")
	in := Config{
		RelayID:  "test-rid",
		Secret:   "test-secret",
		AgentURL: "wss://relay.example/agent/test-rid",
	}
	if err := SaveConfig(path, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch: got %+v want %+v", out, in)
	}
}

func TestLoadConfigRejectsIncompleteFile(t *testing.T) {
	// Only relay_id; missing secret + agent_url → should error.
	path := filepath.Join(t.TempDir(), "relay.token")
	if err := os.WriteFile(path, []byte(`{"relay_id":"only"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Errorf("expected error for incomplete config, got nil")
	}
	if !strings.Contains(err.Error(), "missing required fields") {
		t.Errorf("err = %v, want 'missing required fields'", err)
	}
}

func TestLoadConfigReturnsNotExistWhenAbsent(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "nope.token"))
	if !os.IsNotExist(err) {
		t.Errorf("err = %v, want os.IsNotExist", err)
	}
}
