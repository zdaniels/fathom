package security

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureKeyFileGeneratesNewKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", ".master.key")
	key, err := EnsureKeyFile(path)
	if err != nil {
		t.Fatalf("EnsureKeyFile: %v", err)
	}
	if len(key) != keyLen {
		t.Errorf("key len = %d, want %d", len(key), keyLen)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// 0o600 — owner only.
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("key file perm = %v, want owner-only", perm)
	}
}

func TestEnsureKeyFileReusesExistingKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k")
	first, _ := EnsureKeyFile(path)
	second, err := EnsureKeyFile(path)
	if err != nil {
		t.Fatalf("EnsureKeyFile (re-read): %v", err)
	}
	if string(first) != string(second) {
		t.Error("second EnsureKeyFile should return the same persisted bytes")
	}
}

func TestEnsureKeyFileRejectsCorruptKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.key")
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := EnsureKeyFile(path)
	if err == nil {
		t.Error("short key file should error")
	}
}

func TestDefaultVaultPathHonoursEnv(t *testing.T) {
	t.Setenv("FANTAZM_VAULT_PATH", "/tmp/foo/vault")
	if DefaultVaultPath() != "/tmp/foo/vault" {
		t.Error("FANTAZM_VAULT_PATH override not honoured")
	}
	t.Setenv("FANTAZM_VAULT_KEY", "/tmp/foo/key")
	if DefaultKeyfilePath() != "/tmp/foo/key" {
		t.Error("FANTAZM_VAULT_KEY override not honoured")
	}
}
