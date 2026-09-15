package security

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os/exec"
	"runtime"
	"strings"
)

// This file moves the vault master key OFF disk and into the operating
// system's secret store, so there is no plaintext key file sitting next to the
// encrypted vault for a backup, another account on the host, or an accidental
// commit to scoop up. macOS uses the built-in `security` keychain CLI; Linux
// uses `secret-tool` (libsecret / GNOME Keyring) when it is installed. When
// neither is available the caller falls back to the hardened key file.
//
// The key is stored hex-encoded because both backends deal in strings.

const (
	keychainService = "fantazm-vault"
	keychainAccount = "master-key"
	keychainLabel   = "Fathom vault master key"
)

// keychainProvider names the active backend for diagnostics ("" = none).
func keychainProvider() string {
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("security"); err == nil {
			return "macOS Keychain"
		}
	case "linux":
		if _, err := exec.LookPath("secret-tool"); err == nil {
			return "libsecret (secret-tool)"
		}
	}
	return ""
}

// keychainAvailable reports whether an OS secret store is usable on this host.
func keychainAvailable() bool { return keychainProvider() != "" }

// keychainGetKey returns the stored master key. found is false (with nil err)
// when no key has been stored yet — that is the normal first-run case, not an
// error.
func keychainGetKey() (key []byte, found bool, err error) {
	switch runtime.GOOS {
	case "darwin":
		out, code, runErr := runCapture("security",
			"find-generic-password", "-s", keychainService, "-a", keychainAccount, "-w")
		if runErr != nil {
			// Exit status 44 = "item could not be found": treat as not-present.
			if code == 44 {
				return nil, false, nil
			}
			return nil, false, runErr
		}
		return decodeKeychainKey(out)
	case "linux":
		out, code, runErr := runCapture("secret-tool",
			"lookup", "service", keychainService, "account", keychainAccount)
		if runErr != nil {
			// secret-tool exits non-zero with empty output when the item is absent.
			if strings.TrimSpace(out) == "" && code != 0 {
				return nil, false, nil
			}
			return nil, false, runErr
		}
		if strings.TrimSpace(out) == "" {
			return nil, false, nil
		}
		return decodeKeychainKey(out)
	}
	return nil, false, errors.New("no OS keychain backend on this platform")
}

// keychainSetKey stores (or replaces) the master key.
func keychainSetKey(key []byte) error {
	enc := hex.EncodeToString(key)
	switch runtime.GOOS {
	case "darwin":
		// -U updates the item in place if it already exists.
		_, _, err := runCapture("security",
			"add-generic-password", "-U",
			"-s", keychainService, "-a", keychainAccount,
			"-l", keychainLabel, "-w", enc)
		return err
	case "linux":
		cmd := exec.Command("secret-tool", "store", "--label", keychainLabel,
			"service", keychainService, "account", keychainAccount)
		cmd.Stdin = strings.NewReader(enc)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return wrapExecErr("secret-tool store", err, stderr.String())
		}
		return nil
	}
	return errors.New("no OS keychain backend on this platform")
}

// keychainDeleteKey removes the stored key. Missing is not an error.
func keychainDeleteKey() error {
	switch runtime.GOOS {
	case "darwin":
		_, _, _ = runCapture("security",
			"delete-generic-password", "-s", keychainService, "-a", keychainAccount)
		return nil
	case "linux":
		_, _, _ = runCapture("secret-tool", "clear",
			"service", keychainService, "account", keychainAccount)
		return nil
	}
	return nil
}

func decodeKeychainKey(s string) ([]byte, bool, error) {
	key, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, false, errors.New("keychain holds a malformed vault key (not hex)")
	}
	if len(key) != keyLen {
		return nil, false, errors.New("keychain vault key has wrong length")
	}
	return key, true, nil
}

// runCapture runs a command, returning trimmed stdout, the process exit code
// (-1 when it never started), and an error built from stderr on failure.
func runCapture(name string, args ...string) (string, int, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		code := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		}
		return stdout.String(), code, wrapExecErr(name, err, stderr.String())
	}
	return stdout.String(), 0, nil
}

func wrapExecErr(what string, err error, stderr string) error {
	if s := strings.TrimSpace(stderr); s != "" {
		return errors.New(what + ": " + s)
	}
	return errors.New(what + ": " + err.Error())
}
