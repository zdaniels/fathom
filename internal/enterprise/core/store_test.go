package core

import (
	"path/filepath"
	"sync"
	"testing"
)

func TestPersistentRolesTenantsAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	r, tm, db, err := OpenState(path)
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := tm.Create("Acme", "acme", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.AssignRole("alice", RoleOperator, "admin", tenant.ID); err != nil {
		t.Fatal(err)
	}
	if r.AssignRole("alice", Role("bogus"), "admin", "") == nil {
		t.Fatal("invalid role accepted")
	}
	if r.AssignRole("alice", RoleAdmin, "admin", "missing") == nil {
		t.Fatal("unknown tenant accepted")
	}
	db.Close()
	r, tm, db, err = OpenState(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r.GetRole("alice", tenant.ID) != RoleOperator {
		t.Fatal("lost role")
	}
	if _, ok := tm.Get(tenant.ID); !ok {
		t.Fatal("lost tenant")
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := tm.Create("Same", "same", nil)
			if err == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if success != 1 {
		t.Fatalf("duplicate slug created %d times", success)
	}
}
func TestAssignmentKeysCannotCollide(t *testing.T) {
	r := NewRBACManager()
	r.AssignRole("b:c", RoleAdmin, "a", "a")
	if r.GetRole("c", "a:b") == RoleAdmin {
		t.Fatal("ambiguous identity keys")
	}
}
