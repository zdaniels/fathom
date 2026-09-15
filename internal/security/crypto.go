// Package security provides Fathom's defense layers: input sanitizer, policy
// engine, hash-chained audit log, rate limiter, canary tokens, encrypted
// secrets vault, and the localhost egress proxy that mediates outbound
// HTTPS calls from sandboxed skills.
//
// This file holds the low-level crypto primitives every other security
// component builds on. They mirror the Node implementation exactly so vault
// files and audit chains can be read by either runtime during the rewrite.
package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"

	"golang.org/x/crypto/scrypt"
)

const (
	keyLen    = 32 // AES-256
	saltLen   = 32
	ivLen     = 12 // GCM nonce
	gcmTagLen = 16
)

// scryptParams matches the Node implementation. N=2^17 is the same cost we
// were paying in TS — anyone who unlocks a vault file written by the old
// build will derive the same key here.
var scryptParams = struct {
	N, R, P int
}{N: 1 << 17, R: 8, P: 1}

// GenerateSalt returns a fresh random salt of the canonical length.
func GenerateSalt() ([]byte, error) {
	b := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return nil, err
	}
	return b, nil
}

// DeriveKey runs scrypt over the passphrase and salt to produce the 32-byte
// AES-256 key. Same params as the Node side — vault file portability depends
// on it.
func DeriveKey(passphrase string, salt []byte) ([]byte, error) {
	return scrypt.Key([]byte(passphrase), salt, scryptParams.N, scryptParams.R, scryptParams.P, keyLen)
}

// Encrypt AES-256-GCM encrypts plaintext under the given key. The returned
// blob is layout-identical to the Node version: iv | tag | ciphertext.
func Encrypt(plaintext, key []byte) ([]byte, error) {
	if len(key) != keyLen {
		return nil, errors.New("encryption key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	iv := make([]byte, ivLen)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, ivLen)
	if err != nil {
		return nil, err
	}
	// Go's Seal returns ciphertext || tag. Node wrote iv | tag | ciphertext —
	// we re-split to preserve byte-for-byte compat.
	combined := gcm.Seal(nil, iv, plaintext, nil)
	ciphertext := combined[:len(combined)-gcmTagLen]
	tag := combined[len(combined)-gcmTagLen:]

	out := make([]byte, 0, ivLen+gcmTagLen+len(ciphertext))
	out = append(out, iv...)
	out = append(out, tag...)
	out = append(out, ciphertext...)
	return out, nil
}

// Decrypt reverses Encrypt. Errors when the tag check fails (bad key or
// tampering).
func Decrypt(blob, key []byte) ([]byte, error) {
	if len(key) != keyLen {
		return nil, errors.New("decryption key must be 32 bytes")
	}
	if len(blob) < ivLen+gcmTagLen {
		return nil, errors.New("ciphertext too short")
	}
	iv := blob[:ivLen]
	tag := blob[ivLen : ivLen+gcmTagLen]
	ciphertext := blob[ivLen+gcmTagLen:]

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, ivLen)
	if err != nil {
		return nil, err
	}
	// Recombine to ciphertext || tag for the stdlib Open call.
	combined := append(ciphertext, tag...)
	return gcm.Open(nil, iv, combined, nil)
}

// SHA256Hex returns the hex digest of s — convenience for the audit hash
// chain and token storage.
func SHA256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// SHA256Chain returns sha256(previousHash || data) — the per-entry hash for
// the audit log's tamper-evident chain.
func SHA256Chain(data, previousHash string) string {
	return SHA256Hex(previousHash + data)
}

// RandomBytes is a thin wrapper to make call sites explicit. Returns crypto/rand
// bytes panicking only on system entropy failure (deliberately).
func RandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return nil, err
	}
	return b, nil
}

// GenerateID returns a 32-character hex string — used as session IDs,
// audit entry IDs, generic non-cryptographic identifiers.
func GenerateID() string {
	b, _ := RandomBytes(16)
	return hex.EncodeToString(b)
}

// GenerateToken returns a base64url-encoded random token of 32 bytes — the
// shape Fathom hands out for API tokens and egress proxy invocation tokens.
func GenerateToken() (string, error) {
	b, err := RandomBytes(32)
	if err != nil {
		return "", err
	}
	return base64URLEncode(b), nil
}

// base64URLEncode without padding, matching the Node generateToken output.
func base64URLEncode(b []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	out := make([]byte, 0, ((len(b)+2)/3)*4)
	for i := 0; i < len(b); i += 3 {
		var n uint32
		var pad int
		switch len(b) - i {
		case 1:
			n = uint32(b[i]) << 16
			pad = 2
		case 2:
			n = uint32(b[i])<<16 | uint32(b[i+1])<<8
			pad = 1
		default:
			n = uint32(b[i])<<16 | uint32(b[i+1])<<8 | uint32(b[i+2])
		}
		out = append(out,
			alphabet[(n>>18)&0x3f],
			alphabet[(n>>12)&0x3f],
			alphabet[(n>>6)&0x3f],
			alphabet[n&0x3f],
		)
		out = out[:len(out)-pad]
	}
	return string(out)
}
