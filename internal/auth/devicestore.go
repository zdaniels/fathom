package auth

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/zdaniels/fathom/internal/brandenv"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// DeviceStore persists the auth Manager's token map to disk so that
// restarting `fathom start` doesn't invalidate every paired phone /
// CLI / browser. Single SQLite file alongside the other Fathom DBs
// (~/.fantazm/devices.db).
//
// Schema is one table: every storedToken field plus token_hash as the
// primary key. The raw token is NEVER persisted — only its SHA-256
// hash, same shape as the in-memory map.
//
// last_seen writes batched via a background flusher so a chatty
// device doesn't thrash the disk. Authenticate() updates the
// in-memory copy synchronously (so /devices list is accurate); the
// flusher mirrors to disk every 30s.
type DeviceStore struct {
	db   *sql.DB
	path string

	// Pending last_seen updates, flushed every 30s. Map of
	// tokenHash → most recent UTC timestamp.
	mu      sync.Mutex
	pending map[string]time.Time

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// DefaultDeviceDBPath returns ~/.fantazm/devices.db (override via
// $FANTAZM_DEVICES_DB for tests / non-standard setups).
func DefaultDeviceDBPath() string {
	if env := brandenv.Get("FATHOM_DEVICES_DB"); env != "" {
		return env
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".fantazm", "devices.db")
}

// OpenDeviceStore opens or creates the SQLite store. Idempotent
// schema init + WAL for low-write contention. Spawns a background
// flusher; caller MUST call Close() on shutdown.
func OpenDeviceStore(path string) (*DeviceStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, err
	}
	schema := `
		CREATE TABLE IF NOT EXISTS tokens (
			token_hash  TEXT PRIMARY KEY,
			user_id     TEXT NOT NULL,
			label       TEXT NOT NULL,
			kind        TEXT NOT NULL,
			device_id   TEXT,
			device_name TEXT,
			created_at  TEXT NOT NULL,
			last_seen   TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_tokens_user ON tokens(user_id);
	`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("devices: schema: %w", err)
	}
	s := &DeviceStore{
		db:      db,
		path:    path,
		pending: map[string]time.Time{},
		stopCh:  make(chan struct{}),
	}
	s.wg.Add(1)
	go s.flushLoop()
	return s, nil
}

// Close stops the flusher (after a final sync to disk) and closes the DB.
func (s *DeviceStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	select {
	case <-s.stopCh:
		// already stopped
	default:
		close(s.stopCh)
	}
	s.wg.Wait()
	s.flushOnce()
	return s.db.Close()
}

// Load returns every persisted token. Called once during Manager
// boot to hydrate the in-memory map.
func (s *DeviceStore) Load() (map[string]storedToken, error) {
	rows, err := s.db.Query(
		`SELECT token_hash, user_id, label, kind, device_id, device_name, created_at, last_seen
		 FROM tokens`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]storedToken{}
	for rows.Next() {
		var (
			hash     string
			t        storedToken
			devID    sql.NullString
			devName  sql.NullString
			created  string
			lastSeen string
		)
		if err := rows.Scan(&hash, &t.UserID, &t.Label, &t.Kind,
			&devID, &devName, &created, &lastSeen); err != nil {
			return nil, err
		}
		t.DeviceID = devID.String
		t.DeviceName = devName.String
		if t.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, fmt.Errorf("devices: parse created_at: %w", err)
		}
		if t.LastSeen, err = time.Parse(time.RFC3339Nano, lastSeen); err != nil {
			return nil, fmt.Errorf("devices: parse last_seen: %w", err)
		}
		out[hash] = t
	}
	return out, rows.Err()
}

// Upsert persists a new or replaced token. Called synchronously when
// Manager mints a new token so a crash immediately after pairing
// doesn't lose the device.
func (s *DeviceStore) Upsert(hash string, t storedToken) error {
	if s == nil {
		return nil
	}
	var devID, devName sql.NullString
	if t.DeviceID != "" {
		devID = sql.NullString{String: t.DeviceID, Valid: true}
	}
	if t.DeviceName != "" {
		devName = sql.NullString{String: t.DeviceName, Valid: true}
	}
	_, err := s.db.Exec(
		`INSERT INTO tokens (token_hash, user_id, label, kind, device_id, device_name, created_at, last_seen)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(token_hash) DO UPDATE SET
		   user_id = excluded.user_id,
		   label = excluded.label,
		   kind = excluded.kind,
		   device_id = excluded.device_id,
		   device_name = excluded.device_name,
		   created_at = excluded.created_at,
		   last_seen = excluded.last_seen`,
		hash, t.UserID, t.Label, string(t.Kind), devID, devName,
		t.CreatedAt.Format(time.RFC3339Nano),
		t.LastSeen.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("devices: upsert: %w", err)
	}
	return nil
}

// Delete drops a token by hash. Idempotent — no error if the row
// doesn't exist.
func (s *DeviceStore) Delete(hash string) error {
	if s == nil {
		return nil
	}
	_, err := s.db.Exec(`DELETE FROM tokens WHERE token_hash = ?`, hash)
	if err != nil {
		return fmt.Errorf("devices: delete: %w", err)
	}
	return nil
}

// TouchLastSeen queues a last_seen update for batched flush. Safe to
// call on every authenticated request — the flusher coalesces.
func (s *DeviceStore) TouchLastSeen(hash string, ts time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.pending[hash] = ts
	s.mu.Unlock()
}

// flushLoop syncs queued last_seen updates every 30s.
func (s *DeviceStore) flushLoop() {
	defer s.wg.Done()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			s.flushOnce()
		}
	}
}

// flushOnce drains the pending map and writes all updates in one
// transaction. Errors are logged-silently — last_seen drift is non-
// critical and dropping a few updates is preferable to crashing.
func (s *DeviceStore) flushOnce() {
	s.mu.Lock()
	if len(s.pending) == 0 {
		s.mu.Unlock()
		return
	}
	batch := s.pending
	s.pending = map[string]time.Time{}
	s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return
	}
	for hash, ts := range batch {
		_, _ = tx.Exec(`UPDATE tokens SET last_seen = ? WHERE token_hash = ?`,
			ts.Format(time.RFC3339Nano), hash)
	}
	_ = tx.Commit()
}

// errStoreUnavailable is returned when callers attempt to use a nil
// DeviceStore. Exported via the sentinel pattern so handlers can map
// to a sensible HTTP status if needed.
var errStoreUnavailable = errors.New("device store unavailable")
