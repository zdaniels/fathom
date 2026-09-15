package security

import (
	"github.com/zdaniels/fathom/pkg/types"
	"path/filepath"
	"sync"
	"testing"
)

func TestAuditPersistsConcurrentWritersAndCopies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.db")
	a, err := OpenAuditLogger(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := OpenAuditLogger(path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, logger := range []*AuditLogger{a, b} {
		wg.Add(1)
		go func(l *AuditLogger) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				l.Log("s", "u", types.AuditAuth, map[string]interface{}{"nested": map[string]interface{}{"n": i}}, types.PolicyAllow)
			}
		}(logger)
	}
	wg.Wait()
	if a.Err() != nil || b.Err() != nil {
		t.Fatalf("write errors: %v %v", a.Err(), b.Err())
	}
	a.Close()
	b.Close()
	a, err = OpenAuditLogger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	snap := a.Snapshot()
	if len(snap) != 40 || !a.VerifyChain() {
		t.Fatalf("chain lost records: %d", len(snap))
	}
	snap[0].Detail["nested"].(map[string]interface{})["n"] = "mutated"
	if !a.VerifyChain() {
		t.Fatal("snapshot mutated live chain")
	}
	if _, err = a.db.Exec("UPDATE audit SET record='{}' WHERE seq=1"); err != nil {
		t.Fatal(err)
	}
	if a.VerifyChain() {
		t.Fatal("tampering undetected")
	}
}
func TestAuditStorageFailureIsVisible(t *testing.T) {
	a, err := OpenAuditLogger(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	e := a.Log("s", "u", types.AuditAuth, nil, types.PolicyAllow)
	if a.Err() == nil || e.Hash != "" {
		t.Fatal("failed write appeared successful")
	}
}
