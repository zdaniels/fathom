package core

import "testing"

func TestRBACDefaultIsViewer(t *testing.T) {
	r := NewRBACManager()
	if got := r.GetRole("unknown", ""); got != RoleViewer {
		t.Errorf("unassigned role = %v, want viewer", got)
	}
	// Viewer cannot manage users — that's the gate we rely on most.
	if r.HasPermission("unknown", "", func(p Permissions) bool { return p.ManageUsers }) {
		t.Error("viewer must not have ManageUsers")
	}
}

func TestRBACAssignAndQuery(t *testing.T) {
	r := NewRBACManager()
	r.AssignRole("alice", RoleAdmin, "bootstrap", "")
	if r.GetRole("alice", "") != RoleAdmin {
		t.Error("after assign, alice should be admin")
	}
	if !r.HasPermission("alice", "", func(p Permissions) bool { return p.ManageUsers }) {
		t.Error("admin should have ManageUsers")
	}
	if !r.HasPermission("alice", "", func(p Permissions) bool { return p.ViewAuditLogs }) {
		t.Error("admin should have ViewAuditLogs")
	}
}

func TestRBACTenantScopedAssignmentsAreIsolated(t *testing.T) {
	r := NewRBACManager()
	r.AssignRole("alice", RoleAdmin, "boot", "tenant-a")
	// Same user in another tenant should still default to viewer.
	if r.GetRole("alice", "tenant-b") != RoleViewer {
		t.Error("alice in tenant-b should default to viewer (no cross-tenant leak)")
	}
	if r.GetRole("alice", "") != RoleViewer {
		t.Error("alice global should default to viewer (no scope leak)")
	}
}

func TestRBACOperatorCannotManageUsers(t *testing.T) {
	r := NewRBACManager()
	r.AssignRole("bob", RoleOperator, "boot", "")
	if r.HasPermission("bob", "", func(p Permissions) bool { return p.ManageUsers }) {
		t.Error("operator must not have ManageUsers")
	}
	if !r.HasPermission("bob", "", func(p Permissions) bool { return p.UseAgents }) {
		t.Error("operator should have UseAgents")
	}
}

func TestRBACRemove(t *testing.T) {
	r := NewRBACManager()
	r.AssignRole("carol", RoleAdmin, "boot", "")
	if !r.RemoveRole("carol", "") {
		t.Error("remove returned false for existing assignment")
	}
	if r.RemoveRole("carol", "") {
		t.Error("remove returned true on a no-op")
	}
	if r.GetRole("carol", "") != RoleViewer {
		t.Error("after remove, carol falls back to viewer")
	}
}

func TestRBACListAssignmentsFiltersByTenant(t *testing.T) {
	r := NewRBACManager()
	r.AssignRole("a", RoleAdmin, "boot", "t1")
	r.AssignRole("b", RoleOperator, "boot", "t2")
	r.AssignRole("c", RoleViewer, "boot", "")
	all := r.ListAssignments("")
	if len(all) != 3 {
		t.Errorf("global list len = %d, want 3", len(all))
	}
	t1 := r.ListAssignments("t1")
	if len(t1) != 1 || t1[0].UserID != "a" {
		t.Errorf("t1 filter = %v", t1)
	}
}
