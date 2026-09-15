// Package auth provides bearer-token authentication for the gateway. Tokens
// are stored as SHA-256 hashes (the raw token never persists), keyed by hash
// so lookup is O(1). Bootstraps with a printed-once admin token on first
// boot when no tokens exist.
package auth

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/zdaniels/fathom/internal/security"
)

// Result is what authenticate returns.
type Result struct {
	UserID   string
	Method   string
	DeviceID string // non-empty when the auth token is bound to a paired device
}

// Manager owns the in-memory token store. Optionally backed by a
// DeviceStore for persistence across restarts — when set, every
// Create/Revoke mirrors to disk and last_seen updates batch-flush.
type Manager struct {
	mu     sync.RWMutex
	tokens map[string]storedToken // tokenHash → metadata
	store  *DeviceStore           // nil = ephemeral (legacy behavior)
}

// Kind classifies what minted a token. Matters for the device-list UI
// (we don't want to show the bootstrap admin token as a "device") and
// for audit logging (different events for device vs api use).
type TokenKind string

const (
	KindInitial TokenKind = "initial" // bootstrap admin token printed at first boot
	KindAPI     TokenKind = "api"     // manually-created API token
	KindDevice  TokenKind = "device"  // mobile / web client paired via fathom pair
)

type storedToken struct {
	ExpiresAt  time.Time
	UserID     string
	Label      string
	Kind       TokenKind
	DeviceID   string // only set for KindDevice
	DeviceName string // only set for KindDevice
	CreatedAt  time.Time
	LastSeen   time.Time
}

// New returns an empty in-memory Manager (no disk persistence).
// Existing callers / tests get the same behavior they always had.
func New() *Manager {
	return &Manager{tokens: make(map[string]storedToken)}
}

// NewWithStore returns a Manager backed by a DeviceStore. On
// construction, hydrates the in-memory map from whatever was
// persisted on disk — restart-survival for paired devices and admin
// tokens. Subsequent writes (CreateAPIToken, CreateDeviceToken,
// Revoke*) mirror to the store synchronously; Authenticate updates
// last_seen via the store's batch flusher (low write amplification).
func NewWithStore(store *DeviceStore) (*Manager, error) {
	m := &Manager{
		tokens: make(map[string]storedToken),
		store:  store,
	}
	if store != nil {
		loaded, err := store.Load()
		if err != nil {
			return nil, err
		}
		for hash, t := range loaded {
			m.tokens[hash] = t
		}
	}
	return m, nil
}

// CreateAPIToken mints a fresh token for the given user. Returns the RAW
// token — caller must show it once and discard.
func (m *Manager) CreateAPIToken(userID, label string) (string, error) {
	return m.createToken(userID, label, KindAPI, "", "")
}

// CreateSessionToken creates an ephemeral, expiring SSO session. It is never
// persisted or exchangeable for an unbounded paired-device credential.
func (m *Manager) CreateSessionToken(user string, ttl time.Duration) (string, error) {
	if ttl <= 0 || ttl > time.Hour {
		return "", errors.New("invalid session lifetime")
	}
	token, err := security.GenerateToken()
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for k, v := range m.tokens {
		if !v.ExpiresAt.IsZero() && now.After(v.ExpiresAt) {
			delete(m.tokens, k)
		}
	}
	m.tokens[security.SHA256Hex(token)] = storedToken{UserID: user, Kind: "sso", CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(ttl)}
	return token, nil
}

// CreateDeviceToken mints a token bound to a paired device. deviceID is
// the public identifier the user uses to revoke (`fathom devices revoke
// <id>`); deviceName is the human-readable label shown in the device list.
//
// Returns the raw token — the caller is responsible for handing it back
// to exactly the device that's pairing (the pairing endpoint does this
// over HTTPS).
func (m *Manager) CreateDeviceToken(userID, deviceID, deviceName string) (string, error) {
	if deviceID == "" {
		return "", errors.New("device id required")
	}
	return m.createToken(userID, "device:"+deviceName, KindDevice, deviceID, deviceName)
}

func (m *Manager) createToken(userID, label string, kind TokenKind, deviceID, deviceName string) (string, error) {
	token, err := security.GenerateToken()
	if err != nil {
		return "", err
	}
	hash := security.SHA256Hex(token)
	now := time.Now().UTC()
	stored := storedToken{
		UserID:     userID,
		Label:      label,
		Kind:       kind,
		DeviceID:   deviceID,
		DeviceName: deviceName,
		CreatedAt:  now,
		LastSeen:   now,
	}
	m.mu.Lock()
	m.tokens[hash] = stored
	m.mu.Unlock()
	// Mirror to disk synchronously — losing a freshly-paired device
	// to a crash window seconds after pairing would be a confusing
	// support story.
	if m.store != nil {
		if err := m.store.Upsert(hash, stored); err != nil {
			return "", fmt.Errorf("persist token: %w", err)
		}
	}
	return token, nil
}

// Authenticate validates a presented token. Returns the user it belongs to
// or an error. Updates LastSeen on success — used by `fathom devices list`
// to surface "this iPad hasn't checked in in 14 days, you can probably
// revoke it." LastSeen is mirrored to disk via the store's batched
// flusher (every 30s), not on every call, so a chatty client doesn't
// thrash the disk.
func (m *Manager) Authenticate(token string) (Result, error) {
	if token == "" {
		return Result{}, errors.New("token required")
	}
	hash := security.SHA256Hex(token)
	now := time.Now().UTC()
	m.mu.Lock()
	stored, ok := m.tokens[hash]
	if !ok || (!stored.ExpiresAt.IsZero() && now.After(stored.ExpiresAt)) {
		m.mu.Unlock()
		return Result{}, errors.New("invalid token")
	}
	stored.LastSeen = now
	m.tokens[hash] = stored
	m.mu.Unlock()
	if m.store != nil {
		m.store.TouchLastSeen(hash, now)
	}
	method := "token"
	if stored.Kind == "sso" {
		method = "sso"
	}
	if stored.Kind == KindDevice {
		method = "device"
	}
	return Result{UserID: stored.UserID, Method: method, DeviceID: stored.DeviceID}, nil
}

// HasTokens reports whether any token is registered. Used by the gateway to
// decide whether to print the initial admin token banner.
func (m *Manager) HasTokens() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.tokens) > 0
}

// SetupInitialToken creates a token for userID="admin", labelled
// "initial-setup". The returned token is printed once during boot and never
// shown again. Kind=KindInitial so callers can tell "is this the bootstrap
// token" vs "is this a deliberately-minted API token" — useful for
// `fathom serve --tunnel` which refuses to start with only an initial
// token (too risky over a public URL).
func (m *Manager) SetupInitialToken() (string, error) {
	return m.createToken("admin", "initial-setup", KindInitial, "", "")
}

// HasNonInitialTokens reports whether at least one non-bootstrap token
// exists — either a deliberately-created API token or a paired device.
// Used by `fathom serve --tunnel` to refuse start when the only token
// would be the bootstrap admin token (which would otherwise expose
// admin access over a public URL).
func (m *Manager) HasNonInitialTokens() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, t := range m.tokens {
		if t.Kind != KindInitial {
			return true
		}
	}
	return false
}

// RevokeByHash removes a token by its hash. Used by admin endpoints.
func (m *Manager) RevokeByHash(hash string) bool {
	m.mu.Lock()
	if _, ok := m.tokens[hash]; !ok {
		m.mu.Unlock()
		return false
	}
	delete(m.tokens, hash)
	m.mu.Unlock()
	if m.store != nil {
		_ = m.store.Delete(hash)
	}
	return true
}

// Device is the public view of a paired device — exposed via the
// /api/v1/devices endpoint and the `fathom devices list` CLI.
type Device struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	UserID    string    `json:"user_id"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
}

// ListDevices returns every device-kind token, optionally scoped to a
// userID (empty string = all users). Sorted by CreatedAt descending so
// the most recent pairing shows first.
func (m *Manager) ListDevices(userID string) []Device {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Device
	for _, t := range m.tokens {
		if t.Kind != KindDevice {
			continue
		}
		if userID != "" && t.UserID != userID {
			continue
		}
		out = append(out, Device{
			ID:        t.DeviceID,
			Name:      t.DeviceName,
			UserID:    t.UserID,
			CreatedAt: t.CreatedAt,
			LastSeen:  t.LastSeen,
		})
	}
	// Newest first.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].CreatedAt.After(out[i].CreatedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// RevokeDevice removes the token bound to deviceID. Returns true if
// something was deleted. Idempotent — second call returns false.
func (m *Manager) RevokeDevice(deviceID string) bool {
	if deviceID == "" {
		return false
	}
	m.mu.Lock()
	var revoked string
	for hash, t := range m.tokens {
		if t.Kind == KindDevice && t.DeviceID == deviceID {
			delete(m.tokens, hash)
			revoked = hash
			break
		}
	}
	m.mu.Unlock()
	if revoked == "" {
		return false
	}
	if m.store != nil {
		_ = m.store.Delete(revoked)
	}
	return true
}
