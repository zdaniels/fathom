package security

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/zdaniels/fathom/pkg/types"
)

// PruneBefore removes only a verified contiguous prefix. Its final hash becomes
// the retained chain's checkpoint in the same transaction as deletion.
func (a *AuditLogger) PruneBefore(cutoff time.Time) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return 0, a.err
	}
	if a.db == nil {
		return 0, nil
	}
	tx, err := a.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE audit_lock SET value=value WHERE id=1"); err != nil {
		return 0, err
	}
	var anchor string
	if err = tx.QueryRow("SELECT hash FROM audit_checkpoint WHERE id=1").Scan(&anchor); err != nil {
		return 0, err
	}
	rows, err := tx.Query("SELECT seq,record FROM audit ORDER BY seq")
	if err != nil {
		return 0, err
	}
	var through int64
	count := 0
	prefix := true
	prev := anchor
	checkpoint := anchor
	for rows.Next() {
		var seq int64
		var b []byte
		var entry types.AuditEntry
		if err = rows.Scan(&seq, &b); err != nil {
			rows.Close()
			return 0, err
		}
		if err = json.Unmarshal(b, &entry); err != nil {
			rows.Close()
			return 0, err
		}
		hash, herr := entryHash(entry)
		if herr != nil || hash != entry.Hash || entry.PreviousHash != prev {
			rows.Close()
			return 0, fmt.Errorf("refusing to prune an invalid audit chain")
		}
		prev = entry.Hash
		if prefix && entry.Timestamp.Before(cutoff) {
			through = seq
			count++
			checkpoint = entry.Hash
		} else {
			prefix = false
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, nil
	}
	if _, err = tx.Exec("UPDATE audit_checkpoint SET hash=?,pruned=pruned+? WHERE id=1", checkpoint, count); err != nil {
		return 0, err
	}
	if _, err = tx.Exec("DELETE FROM audit WHERE seq<=?", through); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}
