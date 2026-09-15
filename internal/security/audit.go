package security

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/zdaniels/fathom/pkg/types"
)

// Recorder is the subset of AuditLogger that callers (admin handlers,
// future fathom-gateway proxy code) actually need. Splitting it out lets
// the gateway and llmproxy share the same admin surface without each one
// having to import the concrete AuditLogger or build its own adapter.
//
// AuditLogger satisfies Recorder; any future audit sink (e.g. an OTLP
// streamer, a forwarding shim that writes to multiple stores) only has to
// implement these three methods to drop in.
type Recorder interface {
	Log(sessionID, userID string, action types.AuditAction, detail map[string]interface{}, policyResult types.PolicyDecision) types.AuditEntry
	Snapshot() []types.AuditEntry
	VerifyChain() bool
}

// AuditLogger keeps a tamper-evident, append-only log of security-relevant
// events. Each entry's hash is sha256(serialize(entry-with-no-hash) + previous-hash),
// so any modification to an earlier entry invalidates every subsequent one.
//
// The log is in-memory + bounded (FIFO eviction beyond maxEntries) which
// suits the "fast read for the admin API" path. A future export-to-disk
// hook is available via the OnEntry callback.
type AuditLogger struct {
	mu         sync.Mutex
	entries    []types.AuditEntry
	maxEntries int
	lastHash   string
	OnEntry    func(types.AuditEntry) // optional; called synchronously while the lock is held
}

// NewAuditLogger returns a logger keeping the last maxEntries in memory.
// 10_000 is the default in the TS implementation.
func NewAuditLogger(maxEntries int) *AuditLogger {
	if maxEntries <= 0 {
		maxEntries = 10_000
	}
	return &AuditLogger{
		entries:    make([]types.AuditEntry, 0, 256),
		maxEntries: maxEntries,
	}
}

// Log records an event into the chain. policyResult is the deny/allow/escalate
// decision the rule engine returned (or PolicyAllow if no rule fired).
func (a *AuditLogger) Log(sessionID, userID string, action types.AuditAction, detail map[string]interface{}, policyResult types.PolicyDecision) types.AuditEntry {
	if policyResult == "" {
		policyResult = types.PolicyAllow
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	entry := types.AuditEntry{
		ID:           GenerateID(),
		Timestamp:    time.Now().UTC(),
		SessionID:    sessionID,
		UserID:       userID,
		Action:       action,
		Detail:       detail,
		PolicyResult: policyResult,
		PreviousHash: a.lastHash,
	}
	// Hash everything except the Hash field itself.
	serialized, _ := json.Marshal(struct {
		ID           string                 `json:"id"`
		Timestamp    time.Time              `json:"timestamp"`
		SessionID    string                 `json:"sessionId"`
		UserID       string                 `json:"userId"`
		Action       types.AuditAction      `json:"action"`
		Detail       map[string]interface{} `json:"detail"`
		PolicyResult types.PolicyDecision   `json:"policyResult"`
		PreviousHash string                 `json:"previousHash"`
	}{
		ID:           entry.ID,
		Timestamp:    entry.Timestamp,
		SessionID:    entry.SessionID,
		UserID:       entry.UserID,
		Action:       entry.Action,
		Detail:       entry.Detail,
		PolicyResult: entry.PolicyResult,
		PreviousHash: entry.PreviousHash,
	})
	entry.Hash = SHA256Chain(string(serialized), a.lastHash)
	a.lastHash = entry.Hash

	a.entries = append(a.entries, entry)
	if len(a.entries) > a.maxEntries {
		// Shift the slice; cheap for our size envelope and avoids unbounded growth.
		a.entries = append(a.entries[:0], a.entries[len(a.entries)-a.maxEntries:]...)
	}
	if a.OnEntry != nil {
		a.OnEntry(entry)
	}
	return entry
}

// Snapshot returns a copy of all current entries.
func (a *AuditLogger) Snapshot() []types.AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]types.AuditEntry, len(a.entries))
	copy(out, a.entries)
	return out
}

// VerifyChain recomputes every entry's hash and returns true iff the
// surviving subchain is internally consistent.
//
// We can't anchor against the empty string after FIFO eviction (the first
// retained entry's PreviousHash points at an evicted entry), so we anchor
// at the first surviving entry's stated PreviousHash. The property still
// holds: no entry inside the retained window has been tampered with after
// the fact.
func (a *AuditLogger) VerifyChain() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.entries) == 0 {
		return true
	}
	prev := a.entries[0].PreviousHash
	for _, e := range a.entries {
		serialized, _ := json.Marshal(struct {
			ID           string                 `json:"id"`
			Timestamp    time.Time              `json:"timestamp"`
			SessionID    string                 `json:"sessionId"`
			UserID       string                 `json:"userId"`
			Action       types.AuditAction      `json:"action"`
			Detail       map[string]interface{} `json:"detail"`
			PolicyResult types.PolicyDecision   `json:"policyResult"`
			PreviousHash string                 `json:"previousHash"`
		}{
			ID: e.ID, Timestamp: e.Timestamp, SessionID: e.SessionID, UserID: e.UserID,
			Action: e.Action, Detail: e.Detail, PolicyResult: e.PolicyResult, PreviousHash: e.PreviousHash,
		})
		if SHA256Chain(string(serialized), e.PreviousHash) != e.Hash {
			return false
		}
		if e.PreviousHash != prev {
			return false
		}
		prev = e.Hash
	}
	return true
}
