// Package core holds the deployment-agnostic primitives that team and
// enterprise mode build on: RBAC, multitenancy, and the compliance
// exporter. These are pure managers — no HTTP coupling, no dependency
// on the gateway or chat surfaces — so the fathom-gateway binary can
// reuse them without dragging the agent in.
package core

import (
	"database/sql"
	"encoding/json"
	"sync"
	"time"
)

// Role classifies a user's allowed actions.
type Role string

const (
	RoleAdmin    Role = "admin"
	RoleOperator Role = "operator"
	RoleViewer   Role = "viewer"
)

// Permissions is the flat set of capabilities a role unlocks. Same shape as
// the TS impl so admin endpoints behave identically across the rewrite.
type Permissions struct {
	ManageUsers           bool
	ManageSkills          bool
	ManagePolicies        bool
	ViewAuditLogs         bool
	ViewOwnAuditLogs      bool
	ConfigureAgents       bool
	UseAgents             bool
	InstallApprovedSkills bool
	ViewOwnHistory        bool
	ExecuteTools          bool
}

var rolePermissions = map[Role]Permissions{
	RoleAdmin: {
		ManageUsers: true, ManageSkills: true, ManagePolicies: true,
		ViewAuditLogs: true, ViewOwnAuditLogs: true, ConfigureAgents: true,
		UseAgents: true, InstallApprovedSkills: true, ViewOwnHistory: true,
		ExecuteTools: true,
	},
	RoleOperator: {
		UseAgents: true, ViewOwnAuditLogs: true, InstallApprovedSkills: true,
		ViewOwnHistory: true, ExecuteTools: true,
	},
	RoleViewer: {
		UseAgents: true, ViewOwnHistory: true,
	},
}

// Assignment is one (userID, tenantID?) → Role record.
type Assignment struct {
	UserID     string    `json:"userId"`
	Role       Role      `json:"role"`
	TenantID   string    `json:"tenantId,omitempty"`
	AssignedBy string    `json:"assignedBy"`
	AssignedAt time.Time `json:"assignedAt"`
}

// RBACManager owns the live role map.
type RBACManager struct {
	db          *sql.DB
	mu          sync.RWMutex
	assignments map[string]Assignment
}

// NewRBACManager returns an empty manager. mountEnterprise grants the
// bootstrap admin role immediately after creation.
func NewRBACManager() *RBACManager {
	return &RBACManager{assignments: make(map[string]Assignment)}
}

func keyFor(userID, tenantID string) string {
	b, _ := json.Marshal([]string{tenantID, userID})
	return string(b)
}

// AssignRole sets userID's role. Records who did the assignment for audit.
func (r *RBACManager) AssignRole(userID string, role Role, assignedBy, tenantID string) error {
	if err := validAssignment(userID, role, tenantID); err != nil {
		return err
	}
	a := Assignment{UserID: userID, Role: role, TenantID: tenantID, AssignedBy: assignedBy, AssignedAt: time.Now().UTC()}
	if r.db != nil {
		return r.saveAssignment(a)
	}
	r.mu.Lock()
	r.assignments[keyFor(userID, tenantID)] = a
	r.mu.Unlock()
	return nil
}

// EnsureUser records a first login without overwriting a concurrently granted
// role. The database uniqueness constraint is the cross-process arbiter.
func (r *RBACManager) EnsureUser(user string) error {
	if err := validAssignment(user, RoleViewer, ""); err != nil {
		return err
	}
	a := Assignment{UserID: user, Role: RoleViewer, AssignedBy: "sso", AssignedAt: time.Now().UTC()}
	if r.db != nil {
		b, _ := json.Marshal(a)
		_, err := r.db.Exec("INSERT OR IGNORE INTO roles VALUES(?,?,?)", user, "", b)
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := keyFor(user, "")
	if _, ok := r.assignments[key]; !ok {
		r.assignments[key] = a
	}
	return nil
}

// RemoveRole drops the assignment.
func (r *RBACManager) RemoveRole(userID, tenantID string) bool {
	if r.db != nil {
		res, err := r.db.Exec("DELETE FROM roles WHERE user_id=? AND tenant_id=?", userID, tenantID)
		if err != nil {
			return false
		}
		n, _ := res.RowsAffected()
		return n > 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	k := keyFor(userID, tenantID)
	if _, ok := r.assignments[k]; !ok {
		return false
	}
	delete(r.assignments, k)
	return true
}

// GetRole returns the assigned role; defaults to viewer (the safest fallback).
func (r *RBACManager) GetRole(userID, tenantID string) Role {
	if r.db != nil {
		var b []byte
		var a Assignment
		if err := r.db.QueryRow("SELECT record FROM roles WHERE user_id=? AND tenant_id=?", userID, tenantID).Scan(&b); err != nil {
			return RoleViewer
		}
		if json.Unmarshal(b, &a) != nil {
			return RoleViewer
		}
		return a.Role
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if a, ok := r.assignments[keyFor(userID, tenantID)]; ok {
		return a.Role
	}
	return RoleViewer
}

// HasPermission is the cheap allow/deny check the admin handlers call.
func (r *RBACManager) HasPermission(userID, tenantID string, pred func(Permissions) bool) bool {
	role := r.GetRole(userID, tenantID)
	perms, ok := rolePermissions[role]
	if !ok {
		perms = rolePermissions[RoleViewer]
	}
	return pred(perms)
}

// ListAssignments returns all assignments, optionally filtered by tenant.
func (r *RBACManager) ListAssignments(tenantID string) []Assignment {
	if r.db != nil {
		all, err := r.storedAssignments()
		if err != nil {
			return nil
		}
		out := []Assignment{}
		for _, a := range all {
			if tenantID == "" || a.TenantID == tenantID {
				out = append(out, a)
			}
		}
		return out
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Assignment, 0, len(r.assignments))
	for _, a := range r.assignments {
		if tenantID != "" && a.TenantID != tenantID {
			continue
		}
		out = append(out, a)
	}
	return out
}
