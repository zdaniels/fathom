package security

import (
	"errors"
	"fmt"
	"github.com/zdaniels/fathom/internal/brandenv"
	"os"
	"path/filepath"
	"runtime"
)

// DefaultVaultPath is where Fathom keeps the encrypted secrets blob in
// personal mode. Overridable via FANTAZM_VAULT_PATH.
func DefaultVaultPath() string {
	if v := brandenv.Get("FATHOM_VAULT_PATH"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".fantazm", "vault")
}

// DefaultKeyfilePath holds the 32-byte master key that auto-unlocks the
// vault on personal-mode boots (SSH-agent style). Overridable via
// FANTAZM_VAULT_KEY.
//
// This is now the FALLBACK store: LoadOrCreateMasterKey prefers the OS
// keychain (macOS Keychain / libsecret) so there is no plaintext key on disk;
// the key file is used only when no keychain backend is available or when an
// operator pins FANTAZM_VAULT_KEY explicitly. When the key file is used, its
// permissions are enforced (see secureKeyFilePerms).
func DefaultKeyfilePath() string {
	if v := brandenv.Get("FATHOM_VAULT_KEY"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".fantazm", ".master.key")
}

// LoadOrCreateMasterKey is the preferred way to obtain the vault master key.
// It keeps the key in the OS keychain (macOS Keychain / libsecret) when one is
// available, so no plaintext key file lives on disk; otherwise it falls back
// to the hardened key file. The precedence is:
//
//  1. An explicit FANTAZM_VAULT_KEY path always wins — operators who mount a
//     key file (Docker, k8s secrets) expect it to be honoured verbatim.
//  2. Otherwise, if an OS keychain is available: read it; if empty, migrate an
//     existing legacy key file into the keychain (then delete the plaintext
//     file) or generate a fresh key and store it there.
//  3. Otherwise: the hardened key file (see EnsureKeyFile).
func LoadOrCreateMasterKey() ([]byte, error) {
	if brandenv.Get("FATHOM_VAULT_KEY") != "" {
		return EnsureKeyFile(DefaultKeyfilePath())
	}
	if !keychainAvailable() {
		return EnsureKeyFile(DefaultKeyfilePath())
	}

	key, found, err := keychainGetKey()
	if err != nil {
		// A keychain that is present but erroring (locked, denied) should not
		// silently downgrade to a plaintext file — surface it.
		return nil, fmt.Errorf("reading vault key from %s failed: %w", keychainProvider(), err)
	}
	if found {
		return key, nil
	}

	// First run under the keychain. Migrate a legacy plaintext key file if one
	// exists so existing vaults keep opening, then remove the plaintext copy.
	keyPath := DefaultKeyfilePath()
	if data, ferr := os.ReadFile(keyPath); ferr == nil && len(data) == keyLen {
		if serr := keychainSetKey(data); serr != nil {
			return nil, fmt.Errorf("migrating vault key into %s failed: %w", keychainProvider(), serr)
		}
		if rerr := os.Remove(keyPath); rerr != nil {
			logger.Warn("vault key migrated to keychain but the plaintext key file could not be removed",
				"path", keyPath, "err", rerr)
		} else {
			logger.Info("migrated vault master key from key file into the OS keychain", "store", keychainProvider())
		}
		return data, nil
	}

	// Fresh key, straight into the keychain.
	fresh, gerr := RandomBytes(keyLen)
	if gerr != nil {
		return nil, gerr
	}
	if serr := keychainSetKey(fresh); serr != nil {
		return nil, fmt.Errorf("storing new vault key in %s failed: %w", keychainProvider(), serr)
	}
	logger.Info("generated new vault master key in the OS keychain", "store", keychainProvider())
	return fresh, nil
}

// MasterKeySource returns a human-readable description of where the master key
// lives, for `vault`/`doctor` status output.
func MasterKeySource() string {
	if brandenv.Get("FATHOM_VAULT_KEY") != "" {
		return DefaultKeyfilePath() + " (FANTAZM_VAULT_KEY)"
	}
	if p := keychainProvider(); p != "" {
		if _, found, err := keychainGetKey(); err == nil && found {
			return p
		}
		return p + " (not yet initialized)"
	}
	return DefaultKeyfilePath()
}

// SensitiveLocalPaths returns every on-disk file that holds a Fathom secret
// or grants control of the gateway. This is the canonical list the skill
// sandbox denies (internal/skills) so a malicious skill can't read them off
// disk. Worth noting: with the master key now usually in the OS keychain, the
// raw API token file is often the most valuable on-disk secret left — it grants
// full gateway control — so it MUST be in this set, not just the vault/key.
func SensitiveLocalPaths() []string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".fantazm")

	deviceDB := filepath.Join(dir, "devices.db")
	if env := brandenv.Get("FATHOM_DEVICES_DB"); env != "" {
		deviceDB = env
	}

	// Mirror writeLocalTokenFile (gateway.go): FANTAZM_TOKEN_FILE overrides the
	// default path. Must honor the same override here or the sandbox would deny
	// the default path while leaving the real token file readable.
	apiToken := filepath.Join(dir, "api-token")
	if env := brandenv.Get("FATHOM_TOKEN_FILE"); env != "" {
		apiToken = env
	}

	return []string{
		DefaultVaultPath(),   // encrypted secrets blob
		DefaultKeyfilePath(), // master key (keyfile fallback)
		apiToken,             // raw bearer token for local clients
		deviceDB,             // paired-device + admin token hashes
		deviceDB + "-wal",    // SQLite write-ahead log sidecar
		deviceDB + "-shm",    // SQLite shared-memory sidecar
	}
}

// EnsureKeyFile reads the master key from path, or generates and persists a
// new one with restrictive permissions (0600 on the file, 0700 on the
// parent dir).
func EnsureKeyFile(path string) ([]byte, error) {
	if path == "" {
		path = DefaultKeyfilePath()
	}
	if data, err := os.ReadFile(path); err == nil {
		if len(data) != keyLen {
			return nil, errors.New("vault key file size mismatch (expected 32 bytes)")
		}
		if err := secureKeyFilePerms(path); err != nil {
			return nil, err
		}
		return data, nil
	}
	if err := ensureParentDir(path); err != nil {
		return nil, err
	}
	key, err := RandomBytes(keyLen)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	logger.Info("generated new vault key file", "path", path)
	return key, nil
}

// secureKeyFilePerms ensures the master-key file is not readable by group or
// other. It first tries to self-heal (chmod 0600 on the file, 0700 on its
// parent dir); if the file is STILL group/other-accessible afterward it
// refuses to proceed, because a world-readable master key sitting next to the
// vault means encryption-at-rest buys nothing against other accounts on the
// host.
//
// Note on residual risk: file permissions alone do NOT defend against a
// same-uid process (e.g. a skill subprocess) reading the key. That gap is
// addressed elsewhere — the key is kept in the OS keychain instead of on disk
// (see LoadOrCreateMasterKey / keychain.go), and skill subprocesses run inside
// an OS sandbox that denies the vault + key paths (internal/skills,
// sandbox_isolate.go).
func secureKeyFilePerms(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return nil // transient stat error — don't block boot on it
	}
	if info.Mode().Perm()&0o077 == 0 {
		return nil
	}
	// Best-effort tighten the key file and its parent dir, then re-check.
	_ = os.Chmod(path, 0o600)
	_ = os.Chmod(filepath.Dir(path), 0o700)

	if runtime.GOOS == "windows" {
		// Windows has no POSIX permission bits; os.Chmod only flips the
		// read-only flag and Perm() is not a faithful ACL view, so a hard
		// fail here would be spurious. ACLs default to per-user on Windows.
		return nil
	}
	info, err = os.Stat(path)
	if err != nil {
		return nil
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf(
			"vault key file %s is group/other-accessible (mode %s) and could not be secured; "+
				"refusing to load it — fix with: chmod 600 %s",
			path, info.Mode().Perm(), path)
	}
	logger.Warn("tightened loose permissions on vault key file", "path", path)
	return nil
}
