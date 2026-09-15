package security

import (
	"github.com/zdaniels/fathom/pkg/types"
	"path/filepath"
	"testing"
	"time"
)

func TestRetentionCheckpointSurvivesEmptyLogAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.db")
	a, err := OpenAuditLogger(path)
	if err != nil {
		t.Fatal(err)
	}
	a.Log("s", "u", types.AuditAuth, nil, types.PolicyAllow)
	cutoff := time.Now()
	a.Log("s", "u", types.AuditAuth, nil, types.PolicyAllow)
	if n, err := a.PruneBefore(cutoff); err != nil || n != 1 {
		t.Fatalf("prune %d %v", n, err)
	}
	if !a.VerifyChain() {
		t.Fatal("checkpoint did not anchor retained records")
	}
	if n, err := a.PruneBefore(time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("prune all %d %v", n, err)
	}
	a.Log("s", "u", types.AuditAuth, nil, types.PolicyAllow)
	a.Close()
	a, err = OpenAuditLogger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if len(a.Snapshot()) != 1 || !a.VerifyChain() {
		t.Fatal("restart lost chain")
	}
	_, _ = a.db.Exec("UPDATE audit SET record='{}'")
	if _, err = a.PruneBefore(time.Now().Add(time.Hour)); err == nil {
		t.Fatal("pruned tampered log")
	}
}
