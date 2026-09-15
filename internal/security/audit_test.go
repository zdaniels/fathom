package security

import (
	"testing"

	"github.com/zdaniels/fathom/pkg/types"
)

func TestAuditChainVerifiesAfterAppend(t *testing.T) {
	log := NewAuditLogger(100)
	for i := 0; i < 5; i++ {
		log.Log("sess", "user", types.AuditToolCall,
			map[string]interface{}{"tool": "x", "i": i},
			types.PolicyAllow,
		)
	}
	if !log.VerifyChain() {
		t.Fatal("chain should verify immediately after append")
	}
}

func TestAuditChainBreaksOnTampering(t *testing.T) {
	log := NewAuditLogger(100)
	log.Log("s", "u", types.AuditAuth, map[string]interface{}{}, types.PolicyAllow)
	log.Log("s", "u", types.AuditAuth, map[string]interface{}{}, types.PolicyAllow)
	log.Log("s", "u", types.AuditAuth, map[string]interface{}{}, types.PolicyAllow)

	// Tamper with the middle entry's detail.
	log.entries[1].Detail = map[string]interface{}{"tampered": true}
	if log.VerifyChain() {
		t.Fatal("chain should NOT verify after detail tampering")
	}
}

func TestAuditFIFOBound(t *testing.T) {
	log := NewAuditLogger(3)
	for i := 0; i < 10; i++ {
		log.Log("s", "u", types.AuditToolCall, map[string]interface{}{"i": i}, types.PolicyAllow)
	}
	snap := log.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("snapshot len = %d, want 3 (bound)", len(snap))
	}
	// FIFO eviction: should be entries 7, 8, 9 — chain still valid.
	if !log.VerifyChain() {
		t.Fatal("chain should still verify after eviction")
	}
}
