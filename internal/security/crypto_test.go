package security

import (
	"bytes"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	salt, err := GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt: %v", err)
	}
	if len(salt) != saltLen {
		t.Fatalf("salt len = %d, want %d", len(salt), saltLen)
	}

	key, err := DeriveKey("correct horse battery staple", salt)
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	if len(key) != keyLen {
		t.Fatalf("key len = %d, want %d", len(key), keyLen)
	}

	plaintext := []byte("Fathom — a secure AI agent platform")
	ct, err := Encrypt(plaintext, key)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	pt, err := Decrypt(ct, key)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(pt, plaintext) {
		t.Fatalf("roundtrip mismatch: got %q want %q", pt, plaintext)
	}
}

func TestDecryptWithWrongKeyFails(t *testing.T) {
	saltA, _ := GenerateSalt()
	saltB, _ := GenerateSalt()
	keyA, _ := DeriveKey("password", saltA)
	keyB, _ := DeriveKey("password", saltB)

	ct, _ := Encrypt([]byte("secret"), keyA)
	if _, err := Decrypt(ct, keyB); err == nil {
		t.Fatal("Decrypt with wrong key must error")
	}
}

func TestSHA256ChainIsDeterministic(t *testing.T) {
	a := SHA256Chain("data", "prev")
	b := SHA256Chain("data", "prev")
	if a != b {
		t.Fatalf("chain not deterministic: %q vs %q", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("hex digest len = %d, want 64", len(a))
	}
}

func TestGenerateTokenIsBase64URLNoPad(t *testing.T) {
	tok, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	// 32 bytes → 43 base64-url chars (no padding).
	if len(tok) != 43 {
		t.Fatalf("token len = %d, want 43", len(tok))
	}
	for _, c := range tok {
		ok := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_'
		if !ok {
			t.Fatalf("token has non-base64url char: %q", c)
		}
	}
}

func TestGenerateIDLen(t *testing.T) {
	id := GenerateID()
	if len(id) != 32 {
		t.Fatalf("id len = %d, want 32", len(id))
	}
}
