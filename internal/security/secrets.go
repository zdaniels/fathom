package security

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SecretsVault keeps OAuth tokens, LLM API keys, and other sensitive material
// encrypted at rest. The wire format is JSON with a base64-encoded salt and a
// base64-encoded AES-256-GCM ciphertext of the secrets map — byte-compatible
// with the TypeScript implementation so the existing ~/.fantazm/vault file is
// readable without migration.
type SecretsVault struct {
	mu        sync.Mutex
	secrets   map[string]storedSecret
	masterKey []byte
	salt      []byte
	path      string
}

// storedSecret is the per-entry record. allowedSkills nil/empty means any
// caller can read; populated means only those skills are authorised.
type storedSecret struct {
	Value         string    `json:"value"`
	AllowedSkills []string  `json:"allowedSkills"`
	CreatedAt     time.Time `json:"createdAt"`
}

// vaultFileFormat is the on-disk JSON shape, version-prefixed for future
// migrations.
type vaultFileFormat struct {
	Version int    `json:"version"`
	Salt    string `json:"salt"`
	Data    string `json:"data"`
}

const vaultFormatVersion = 1

// VaultOpenOptions controls how a vault is opened. Provide either Passphrase
// (derive key via scrypt) or Key (raw 32-byte material — the auto-unlock
// keyfile path supplies this).
type VaultOpenOptions struct {
	Path       string
	Passphrase string
	Key        []byte
}

// OpenVault opens an existing vault file or creates a new one. The salt is
// persisted in the file so the derived key is stable across restarts when
// the same passphrase/key is supplied.
func OpenVault(opts VaultOpenOptions) (*SecretsVault, error) {
	v := &SecretsVault{
		secrets: make(map[string]storedSecret),
		path:    opts.Path,
	}

	if opts.Path != "" {
		if _, err := os.Stat(opts.Path); err == nil {
			data, err := os.ReadFile(opts.Path)
			if err != nil {
				return nil, fmt.Errorf("read vault file: %w", err)
			}
			var parsed vaultFileFormat
			if err := json.Unmarshal(data, &parsed); err != nil {
				return nil, fmt.Errorf(
					"vault file at %s is corrupted (not valid JSON): %w. Restore from backup or remove the file and re-import secrets with 'fathom vault import-env'",
					opts.Path, err,
				)
			}
			if parsed.Version != vaultFormatVersion {
				return nil, fmt.Errorf("unsupported vault file version: %d", parsed.Version)
			}
			v.salt, err = base64.StdEncoding.DecodeString(parsed.Salt)
			if err != nil {
				return nil, fmt.Errorf("vault salt malformed: %w", err)
			}
			key, err := resolveKey(opts, v.salt)
			if err != nil {
				return nil, err
			}
			v.masterKey = key
			ciphertext, err := base64.StdEncoding.DecodeString(parsed.Data)
			if err != nil {
				return nil, fmt.Errorf("vault data malformed: %w", err)
			}
			plaintext, err := Decrypt(ciphertext, v.masterKey)
			if err != nil {
				v.masterKey = nil
				v.salt = nil
				return nil, fmt.Errorf("failed to decrypt vault — wrong passphrase or corrupted file: %w", err)
			}
			if err := json.Unmarshal(plaintext, &v.secrets); err != nil {
				return nil, fmt.Errorf("vault content malformed: %w", err)
			}
			logger.Info("vault opened", "path", opts.Path, "count", len(v.secrets))
			return v, nil
		}
	}

	// Fresh vault — generate salt, derive key, persist (if a path is set).
	salt, err := GenerateSalt()
	if err != nil {
		return nil, err
	}
	v.salt = salt
	key, err := resolveKey(opts, salt)
	if err != nil {
		return nil, err
	}
	v.masterKey = key
	if opts.Path != "" {
		if err := ensureParentDir(opts.Path); err != nil {
			return nil, err
		}
		if err := v.persistLocked(); err != nil {
			return nil, err
		}
		logger.Info("vault created", "path", opts.Path)
	}
	return v, nil
}

func resolveKey(opts VaultOpenOptions, salt []byte) ([]byte, error) {
	if opts.Key != nil {
		if len(opts.Key) != keyLen {
			return nil, fmt.Errorf("vault key must be 32 bytes, got %d", len(opts.Key))
		}
		return opts.Key, nil
	}
	if opts.Passphrase != "" {
		return DeriveKey(opts.Passphrase, salt)
	}
	return nil, errors.New("vault open requires either Passphrase or Key")
}

func ensureParentDir(filePath string) error {
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(dir, 0o700) // best-effort on non-POSIX
	return nil
}

// IsUnlocked reports whether the master key is present in memory.
func (v *SecretsVault) IsUnlocked() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.masterKey != nil
}

// Lock wipes the master key. Subsequent Get / Set will error until Open is
// called again.
func (v *SecretsVault) Lock() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.masterKey = nil
}

// Has reports whether the named secret exists in the vault.
func (v *SecretsVault) Has(name string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, ok := v.secrets[name]
	return ok
}

// Set stores a secret, scoped to the listed skill names (empty list = any
// caller). Persists atomically when the vault is file-backed.
func (v *SecretsVault) Set(name, value string, allowedSkills []string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.masterKey == nil {
		return errors.New("vault locked")
	}
	if allowedSkills == nil {
		allowedSkills = []string{}
	}
	v.secrets[name] = storedSecret{
		Value:         value,
		AllowedSkills: allowedSkills,
		CreatedAt:     time.Now().UTC(),
	}
	if err := v.persistLocked(); err != nil {
		return err
	}
	logger.Info("secret stored", "name", name, "allowedSkills", allowedSkills)
	return nil
}

// Get returns the secret value, enforcing the per-skill scope when
// requestingSkill is non-empty AND the entry has allowedSkills configured.
// Pass "" for requestingSkill to skip the scope check — host-side resolution
// (e.g. the LLM API key) uses this path.
func (v *SecretsVault) Get(name, requestingSkill string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.masterKey == nil {
		return "", errors.New("vault locked")
	}
	stored, ok := v.secrets[name]
	if !ok {
		return "", fmt.Errorf("secret not found: %s", name)
	}
	if len(stored.AllowedSkills) > 0 && requestingSkill != "" {
		allowed := false
		for _, s := range stored.AllowedSkills {
			if s == requestingSkill {
				allowed = true
				break
			}
		}
		if !allowed {
			logger.Warn("secret access denied", "name", name, "requestingSkill", requestingSkill)
			return "", fmt.Errorf("skill %q is not authorized to access secret %q", requestingSkill, name)
		}
	}
	return stored.Value, nil
}

// Delete removes a secret. Returns true if it existed.
func (v *SecretsVault) Delete(name string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.secrets[name]; !ok {
		return false
	}
	delete(v.secrets, name)
	_ = v.persistLocked()
	logger.Info("secret deleted", "name", name)
	return true
}

// SecretEntry is what List returns — name + metadata, NOT the value.
type SecretEntry struct {
	Name          string
	CreatedAt     time.Time
	AllowedSkills []string
}

// List returns metadata for every stored secret, in arbitrary order.
func (v *SecretsVault) List() []SecretEntry {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]SecretEntry, 0, len(v.secrets))
	for name, s := range v.secrets {
		out = append(out, SecretEntry{
			Name:          name,
			CreatedAt:     s.CreatedAt,
			AllowedSkills: append([]string{}, s.AllowedSkills...),
		})
	}
	return out
}

// InjectForRequest resolves the named secrets for a skill invocation. Missing
// or unauthorised secrets are silently omitted — the skill's runner sees a
// "secret not provisioned" error when it actually tries to use one it isn't
// entitled to.
func (v *SecretsVault) InjectForRequest(names []string, skill string) map[string]string {
	out := make(map[string]string)
	for _, name := range names {
		val, err := v.Get(name, skill)
		if err == nil {
			out[name] = val
		} else {
			logger.Warn("failed to inject secret", "name", name, "skill", skill, "err", err)
		}
	}
	return out
}

// persistLocked writes the vault to disk atomically: stage to <path>.tmp,
// then rename. A crash mid-write leaves either the old vault or the new —
// never a truncated file. Caller must hold v.mu.
func (v *SecretsVault) persistLocked() error {
	if v.path == "" || v.masterKey == nil || v.salt == nil {
		return nil
	}
	plaintext, err := json.Marshal(v.secrets)
	if err != nil {
		return err
	}
	ciphertext, err := Encrypt(plaintext, v.masterKey)
	if err != nil {
		return err
	}
	file := vaultFileFormat{
		Version: vaultFormatVersion,
		Salt:    base64.StdEncoding.EncodeToString(v.salt),
		Data:    base64.StdEncoding.EncodeToString(ciphertext),
	}
	body, err := json.Marshal(file)
	if err != nil {
		return err
	}
	tmp := v.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	_ = os.Chmod(tmp, 0o600)
	if err := os.Rename(tmp, v.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
