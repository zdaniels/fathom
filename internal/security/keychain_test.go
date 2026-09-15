package security

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSensitiveLocalPaths guards against regressing the set the skill sandbox
// denies — the raw API token and device-store DB must be in it, not just the
// vault + key (the token is the most valuable on-disk secret once the master
// key moves to the keychain).
func TestSensitiveLocalPaths(t *testing.T) {
	joined := strings.Join(SensitiveLocalPaths(), "\n")
	for _, want := range []string{"vault", ".master.key", "api-token", "devices.db", "devices.db-wal"} {
		if !strings.Contains(joined, want) {
			t.Errorf("SensitiveLocalPaths missing %q; got:\n%s", want, joined)
		}
	}
}

// TestSensitiveLocalPathsHonorsEnvOverrides ensures the set tracks the same
// path overrides the writers use — otherwise the sandbox would deny the
// default path while the real secret file (at the overridden path) stays
// readable.
func TestSensitiveLocalPathsHonorsEnvOverrides(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "custom-token")
	dbPath := filepath.Join(t.TempDir(), "custom-devices.db")
	t.Setenv("FANTAZM_TOKEN_FILE", tokenPath)
	t.Setenv("FANTAZM_DEVICES_DB", dbPath)

	joined := strings.Join(SensitiveLocalPaths(), "\n")
	for _, want := range []string{tokenPath, dbPath, dbPath + "-wal"} {
		if !strings.Contains(joined, want) {
			t.Errorf("SensitiveLocalPaths should honor env override %q; got:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, filepath.Join(".fantazm", "api-token")) {
		t.Error("default api-token path should not appear when FANTAZM_TOKEN_FILE is set")
	}
}

func TestDecodeKeychainKey(t *testing.T) {
	good := hex.EncodeToString(make([]byte, keyLen))
	if _, found, err := decodeKeychainKey(good); err != nil || !found {
		t.Errorf("valid key should decode: found=%v err=%v", found, err)
	}
	if _, _, err := decodeKeychainKey("nothex!!"); err == nil {
		t.Error("non-hex should error")
	}
	if _, _, err := decodeKeychainKey(hex.EncodeToString([]byte("short"))); err == nil {
		t.Error("wrong-length key should error")
	}
}

func TestKeychainProviderMatchesPlatform(t *testing.T) {
	// We don't assert a specific value (depends on the host), only that the
	// availability flag agrees with the provider string.
	if keychainAvailable() != (keychainProvider() != "") {
		t.Error("keychainAvailable must agree with keychainProvider")
	}
}

// TestLoadOrCreateMasterKeyExplicitPath verifies the precedence rule that an
// explicit FANTAZM_VAULT_KEY always wins and round-trips through a key file —
// without ever touching the real OS keychain (which would clobber the user's
// actual vault key).
func TestLoadOrCreateMasterKeyExplicitPath(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), ".master.key")
	t.Setenv("FANTAZM_VAULT_KEY", keyPath)

	k1, err := LoadOrCreateMasterKey()
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if len(k1) != keyLen {
		t.Fatalf("key length = %d, want %d", len(k1), keyLen)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("key file should have been created at %s: %v", keyPath, err)
	}
	// Stable across calls.
	k2, err := LoadOrCreateMasterKey()
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if hex.EncodeToString(k1) != hex.EncodeToString(k2) {
		t.Error("key should be stable across loads with an explicit path")
	}
}

// TestSecureKeyFilePermsRejectsLoose confirms the H2 hardening: a key file
// that cannot be secured is refused rather than used. POSIX-only.
func TestSecureKeyFilePermsRejectsLoose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission semantics only")
	}
	keyPath := filepath.Join(t.TempDir(), ".master.key")
	if err := os.WriteFile(keyPath, make([]byte, keyLen), 0o644); err != nil {
		t.Fatal(err)
	}
	// secureKeyFilePerms should self-heal a chmod-able file back to 0600.
	if err := secureKeyFilePerms(keyPath); err != nil {
		t.Fatalf("self-heal should succeed for an owned file: %v", err)
	}
	info, _ := os.Stat(keyPath)
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("perms not tightened: %s", info.Mode().Perm())
	}
}
