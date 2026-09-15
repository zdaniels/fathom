package security

import (
	"path/filepath"
	"testing"
)

func TestVaultPersistsAcrossOpenWithSameKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vault")
	key, _ := RandomBytes(32)

	v1, err := OpenVault(VaultOpenOptions{Path: path, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	if err := v1.Set("API_KEY", "sk-secret-123", nil); err != nil {
		t.Fatal(err)
	}
	v1.Lock()

	v2, err := OpenVault(VaultOpenOptions{Path: path, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	got, err := v2.Get("API_KEY", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "sk-secret-123" {
		t.Errorf("got %q, want sk-secret-123", got)
	}
}

func TestVaultPerSkillScopeDeniesUnauthorisedReads(t *testing.T) {
	v, _ := OpenVault(VaultOpenOptions{Key: bytes32(0x07)})
	v.Set("GMAIL_TOKEN", "secret", []string{"gmail"})

	if _, err := v.Get("GMAIL_TOKEN", "gmail"); err != nil {
		t.Errorf("gmail should be allowed: %v", err)
	}
	if _, err := v.Get("GMAIL_TOKEN", "github"); err == nil {
		t.Error("github should be denied")
	}
	if _, err := v.Get("GMAIL_TOKEN", ""); err != nil {
		t.Errorf("host-side (empty skill) should be allowed: %v", err)
	}
}

func TestVaultRejectsWrongKeyOnReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vault")

	v1, _ := OpenVault(VaultOpenOptions{Path: path, Key: bytes32(0x11)})
	v1.Set("X", "y", nil)
	v1.Lock()

	if _, err := OpenVault(VaultOpenOptions{Path: path, Key: bytes32(0x22)}); err == nil {
		t.Fatal("reopen with wrong key must error")
	}
}

func bytes32(v byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = v
	}
	return out
}
