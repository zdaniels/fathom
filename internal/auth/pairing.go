// Pairing lets a phone (or any new device with a browser) trade a
// short-lived numeric code for a long-lived device token.
//
// Flow:
//  1. CLI (or admin UI) calls Generate to mint a 6-digit code + record
//     who initiated. Code TTL is short (default 60s).
//  2. CLI displays the code as text + QR. QR encodes
//     https://<host>/?pair_code=<code> so phones that scan land directly
//     on the chat UI with the pairing form pre-filled.
//  3. Phone calls Claim(code, deviceName). Store validates (exists, not
//     expired, not already claimed), mints a device-kind token via the
//     auth Manager, and signals any waiters.
//  4. CLI long-polls via Wait — returns when the code is claimed (or
//     errors when it expires unclaimed).
//
// Codes are one-shot: after Claim succeeds, the code is invalidated.
// Expired codes are swept every 30s by a background goroutine.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"
)

// PairingCode is one in-flight pairing attempt. Returned by Generate to
// the initiator (CLI); the same struct is updated in-place when claimed,
// so a Wait caller holding a reference sees the populated DeviceID etc.
type PairingCode struct {
	Code       string    `json:"code"`
	UserID     string    `json:"user_id"`
	ExpiresAt  time.Time `json:"expires_at"`
	ClaimedAt  time.Time `json:"claimed_at,omitempty"`
	DeviceID   string    `json:"device_id,omitempty"`
	DeviceName string    `json:"device_name,omitempty"`

	// claimSignal is closed when the code is claimed. Long-poll watchers
	// select on this + their context. Buffered-channel-of-zero pattern.
	claimSignal chan struct{}
}

// IsClaimed reports whether this code has been redeemed.
func (p *PairingCode) IsClaimed() bool { return !p.ClaimedAt.IsZero() }

// Sentinel errors so handlers can map to HTTP status codes precisely.
var (
	ErrPairingExpired    = errors.New("pairing code expired or not found")
	ErrPairingClaimed    = errors.New("pairing code already claimed")
	ErrPairingNoSuchCode = errors.New("no such pairing code")
)

// PairingStore is an in-memory pool of pending pairing codes. Safe for
// concurrent use. Codes auto-expire; nothing persists across restart
// (intentional — restarting Fathom should invalidate any in-flight
// pairing).
type PairingStore struct {
	mu      sync.Mutex
	codes   map[string]*PairingCode
	ttl     time.Duration
	manager *Manager

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewPairingStore wires a store onto an auth.Manager. TTL ≤ 0 falls back
// to 60 seconds, the right balance between "type-in time" and
// "small attack window."
func NewPairingStore(m *Manager, ttl time.Duration) *PairingStore {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	s := &PairingStore{
		codes:   make(map[string]*PairingCode),
		ttl:     ttl,
		manager: m,
		stopCh:  make(chan struct{}),
	}
	s.wg.Add(1)
	go s.sweepLoop()
	return s
}

// Close stops the background sweeper. Safe to call multiple times.
func (s *PairingStore) Close() {
	select {
	case <-s.stopCh:
		// already closed
	default:
		close(s.stopCh)
	}
	s.wg.Wait()
}

// Generate mints a new 6-digit code bound to userID (the initiator —
// every device paired against this code joins userID's account).
func (s *PairingStore) Generate(userID string) (*PairingCode, error) {
	code, err := newPairingCode()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	pc := &PairingCode{
		Code:        code,
		UserID:      userID,
		ExpiresAt:   now.Add(s.ttl),
		claimSignal: make(chan struct{}),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Vanishingly rare collision (1 in 1M with a 60s window) — regen.
	for tries := 0; tries < 5; tries++ {
		if _, exists := s.codes[pc.Code]; !exists {
			s.codes[pc.Code] = pc
			return pc, nil
		}
		c, err := newPairingCode()
		if err != nil {
			return nil, err
		}
		pc.Code = c
	}
	return nil, errors.New("pairing: too many code collisions, try again")
}

// Claim redeems a pairing code for a fresh device token. deviceName is
// what the user sees in `fathom devices list`; passing empty falls back
// to "Unnamed device" (the web UI provides a sensible default per UA).
//
// Returns the raw token (one-shot; caller hands to the device) and the
// device record. The code is invalidated on success.
func (s *PairingStore) Claim(code, deviceName string) (Device, string, error) {
	if code == "" {
		return Device{}, "", ErrPairingNoSuchCode
	}
	if deviceName == "" {
		deviceName = "Unnamed device"
	}
	s.mu.Lock()
	pc, ok := s.codes[code]
	if !ok {
		s.mu.Unlock()
		return Device{}, "", ErrPairingNoSuchCode
	}
	now := time.Now().UTC()
	if now.After(pc.ExpiresAt) {
		delete(s.codes, code)
		s.mu.Unlock()
		return Device{}, "", ErrPairingExpired
	}
	if pc.IsClaimed() {
		s.mu.Unlock()
		return Device{}, "", ErrPairingClaimed
	}

	// Mint deviceID + token. We do this while holding the mutex so a
	// concurrent Claim for the same code can't race past the IsClaimed
	// check. Token minting is cheap.
	deviceID := newDeviceID()
	token, err := s.manager.CreateDeviceToken(pc.UserID, deviceID, deviceName)
	if err != nil {
		s.mu.Unlock()
		return Device{}, "", fmt.Errorf("mint device token: %w", err)
	}

	pc.ClaimedAt = now
	pc.DeviceID = deviceID
	pc.DeviceName = deviceName
	close(pc.claimSignal) // wake watchers
	s.mu.Unlock()

	dev := Device{
		ID:        deviceID,
		Name:      deviceName,
		UserID:    pc.UserID,
		CreatedAt: now,
		LastSeen:  now,
	}
	return dev, token, nil
}

// Wait blocks until the code is claimed, expires, or ctx is done. On
// claim returns the device record (no token — only Claim returns the
// raw token). Expiry → ErrPairingExpired. Context done → ctx.Err().
//
// Designed for HTTP long-poll: the CLI calls /pair/watch with a code,
// the handler calls Wait, the response writes when this returns.
func (s *PairingStore) Wait(ctx context.Context, code string) (Device, error) {
	s.mu.Lock()
	pc, ok := s.codes[code]
	if !ok {
		s.mu.Unlock()
		return Device{}, ErrPairingNoSuchCode
	}
	if pc.IsClaimed() {
		s.mu.Unlock()
		return s.snapshotDevice(pc), nil
	}
	signal := pc.claimSignal
	exp := pc.ExpiresAt
	s.mu.Unlock()

	// Race: signal (claimed), expiry timer, context cancel.
	timer := time.NewTimer(time.Until(exp))
	defer timer.Stop()
	select {
	case <-signal:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.snapshotDevice(pc), nil
	case <-timer.C:
		s.mu.Lock()
		delete(s.codes, code)
		s.mu.Unlock()
		return Device{}, ErrPairingExpired
	case <-ctx.Done():
		return Device{}, ctx.Err()
	}
}

// snapshotDevice builds a Device from a (claimed) PairingCode. Caller
// must hold the mutex.
func (s *PairingStore) snapshotDevice(pc *PairingCode) Device {
	return Device{
		ID:        pc.DeviceID,
		Name:      pc.DeviceName,
		UserID:    pc.UserID,
		CreatedAt: pc.ClaimedAt,
		LastSeen:  pc.ClaimedAt,
	}
}

// sweepLoop reaps expired codes every 30s. Keeps the map from growing
// when codes are generated but never claimed (the common case).
func (s *PairingStore) sweepLoop() {
	defer s.wg.Done()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case now := <-t.C:
			s.mu.Lock()
			for c, pc := range s.codes {
				if now.After(pc.ExpiresAt) {
					delete(s.codes, c)
				}
			}
			s.mu.Unlock()
		}
	}
}

// newPairingCode returns a uniformly-random 6-digit numeric string
// (leading zeros preserved). 1M possibilities × 60s TTL × 5 attempts/min
// rate limit = vanishingly small brute-force probability.
func newPairingCode() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	n := binary.BigEndian.Uint32(b[:]) % 1_000_000
	return fmt.Sprintf("%06d", n), nil
}

// newDeviceID returns a short, copy-pasteable identifier for the
// `fathom devices list` UX. Format: 12 lower-hex chars (~48 bits).
func newDeviceID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x", b)
}
