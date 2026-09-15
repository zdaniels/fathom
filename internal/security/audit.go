package security

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zdaniels/fathom/pkg/types"
	_ "modernc.org/sqlite"
)

type Recorder interface {
	Log(sessionID, userID string, action types.AuditAction, detail map[string]interface{}, policyResult types.PolicyDecision) types.AuditEntry
	Snapshot() []types.AuditEntry
	VerifyChain() bool
}

// AuditLogger stores a hash chain. Production loggers use SQLite transactions;
// the bounded in-memory variant is for tests and embedded callers. Err is sticky:
// an I/O or serialization failure must stop privileged work until restart.
type AuditLogger struct {
	mu         sync.Mutex
	entries    []types.AuditEntry
	maxEntries int
	lastHash   string
	OnEntry    func(types.AuditEntry)
	db         *sql.DB
	err        error
}

func NewAuditLogger(max int) *AuditLogger {
	if max <= 0 {
		max = 10000
	}
	return &AuditLogger{maxEntries: max, entries: []types.AuditEntry{}}
}
func AuditPath(dataDir string) string { return filepath.Join(dataDir, "audit.db") }
func OpenAuditLogger(path string) (*AuditLogger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// A write is acquired before reading the chain head, so concurrent processes
	// cannot append two different successors to the same entry.
	_, err = db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL;
 CREATE TABLE IF NOT EXISTS audit (seq INTEGER PRIMARY KEY AUTOINCREMENT, record BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS audit_lock (id INTEGER PRIMARY KEY, value INTEGER NOT NULL);
 INSERT OR IGNORE INTO audit_lock VALUES(1,0);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	a := NewAuditLogger(10000)
	a.db = db
	if !a.VerifyChain() {
		db.Close()
		return nil, fmt.Errorf("audit chain is invalid: %v", a.Err())
	}
	return a, nil
}
func (a *AuditLogger) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.db != nil {
		return a.db.Close()
	}
	return nil
}
func (a *AuditLogger) Err() error { a.mu.Lock(); defer a.mu.Unlock(); return a.err }
func cloneEntry(e types.AuditEntry) (types.AuditEntry, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return e, err
	}
	var out types.AuditEntry
	err = json.Unmarshal(b, &out)
	return out, err
}
func entryHash(e types.AuditEntry) (string, error) {
	// Keep the original v1 serialization order for compatibility.
	b, err := json.Marshal(struct {
		ID           string                 `json:"id"`
		Timestamp    time.Time              `json:"timestamp"`
		SessionID    string                 `json:"sessionId"`
		UserID       string                 `json:"userId"`
		Action       types.AuditAction      `json:"action"`
		Detail       map[string]interface{} `json:"detail"`
		PolicyResult types.PolicyDecision   `json:"policyResult"`
		PreviousHash string                 `json:"previousHash"`
	}{e.ID, e.Timestamp, e.SessionID, e.UserID, e.Action, e.Detail, e.PolicyResult, e.PreviousHash})
	if err != nil {
		return "", err
	}
	return SHA256Chain(string(b), e.PreviousHash), nil
}
func (a *AuditLogger) Log(session, user string, action types.AuditAction, detail map[string]interface{}, decision types.PolicyDecision) types.AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return types.AuditEntry{}
	}
	if decision == "" {
		decision = types.PolicyAllow
	}
	e, err := cloneEntry(types.AuditEntry{ID: GenerateID(), Timestamp: time.Now().UTC(), SessionID: session, UserID: user, Action: action, Detail: detail, PolicyResult: decision, PreviousHash: a.lastHash})
	if err != nil {
		a.err = err
		return types.AuditEntry{}
	}
	var tx *sql.Tx
	if a.db != nil {
		tx, err = a.db.Begin()
		if err != nil {
			a.err = err
			return types.AuditEntry{}
		}
		defer tx.Rollback()
		if _, err = tx.Exec("UPDATE audit_lock SET value=value WHERE id=1"); err != nil {
			a.err = err
			return types.AuditEntry{}
		}
		var b []byte
		err = tx.QueryRow("SELECT record FROM audit ORDER BY seq DESC LIMIT 1").Scan(&b)
		if err != nil && err != sql.ErrNoRows {
			a.err = err
			return types.AuditEntry{}
		}
		if err == nil {
			var last types.AuditEntry
			if err = json.Unmarshal(b, &last); err != nil {
				a.err = err
				return types.AuditEntry{}
			}
			e.PreviousHash = last.Hash
		}
	}
	e.Hash, err = entryHash(e)
	if err != nil {
		a.err = err
		return types.AuditEntry{}
	}
	if tx != nil {
		b, _ := json.Marshal(e)
		if _, err = tx.Exec("INSERT INTO audit(record) VALUES(?)", b); err != nil {
			a.err = err
			return types.AuditEntry{}
		}
		if err = tx.Commit(); err != nil {
			a.err = err
			return types.AuditEntry{}
		}
	} else {
		a.entries = append(a.entries, e)
		if len(a.entries) > a.maxEntries {
			a.entries = append(a.entries[:0], a.entries[len(a.entries)-a.maxEntries:]...)
		}
	}
	a.lastHash = e.Hash
	if a.OnEntry != nil {
		copy, _ := cloneEntry(e)
		a.OnEntry(copy)
	}
	copy, _ := cloneEntry(e)
	return copy
}
func (a *AuditLogger) snapshotLocked() []types.AuditEntry {
	if a.db != nil {
		rows, err := a.db.Query("SELECT record FROM audit ORDER BY seq")
		if err != nil {
			a.err = err
			return nil
		}
		defer rows.Close()
		out := []types.AuditEntry{}
		for rows.Next() {
			var b []byte
			var e types.AuditEntry
			if err = rows.Scan(&b); err != nil {
				a.err = err
				return nil
			}
			if err = json.Unmarshal(b, &e); err != nil {
				a.err = err
				return nil
			}
			out = append(out, e)
		}
		if err = rows.Err(); err != nil {
			a.err = err
			return nil
		}
		return out
	}
	out := make([]types.AuditEntry, len(a.entries))
	for i, e := range a.entries {
		copy, err := cloneEntry(e)
		if err != nil {
			a.err = err
			return nil
		}
		out[i] = copy
	}
	return out
}
func (a *AuditLogger) Snapshot() []types.AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.snapshotLocked()
}
func (a *AuditLogger) VerifyChain() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	entries := a.snapshotLocked()
	if a.err != nil {
		return false
	}
	prev := ""
	if a.db == nil && len(entries) > 0 {
		prev = entries[0].PreviousHash
	}
	for _, e := range entries {
		hash, err := entryHash(e)
		if err != nil || e.PreviousHash != prev || hash != e.Hash {
			return false
		}
		prev = e.Hash
	}
	return true
}
