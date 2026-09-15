// Package core holds the deployment-agnostic primitives that team and
// enterprise mode build on: RBAC, multitenancy, and the compliance
// exporter. These are pure managers — no HTTP coupling, no dependency
// on the gateway or chat surfaces — so the fathom-gateway binary can
// reuse them without dragging the agent in.
package core

import (
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
	mu          sync.RWMutex
	assignments map[string]Assignment
}

// NewRBACManager returns an empty manager. mountEnterprise grants the
// bootstrap admin role immediately after creation.
func NewRBACManager() *RBACManager {
	return &RBACManager{assignments: make(map[string]Assignment)}
}

func keyFor(userID, tenantID string) string {
	if tenantID == "" {
		return userID
	}
	return tenantID + ":" + userID
}

// AssignRole sets userID's role. Records who did the assignment for audit.
func (r *RBACManager) AssignRole(userID string, role Role, assignedBy, tenantID string) {
	r.mu.Lock()
	r.assignments[keyFor(userID, tenantID)] = Assignment{
		UserID: userID, Role: role, TenantID: tenantID,
		AssignedBy: assignedBy, AssignedAt: time.Now().UTC(),
	}
	r.mu.Unlock()
}

// RemoveRole drops the assignment.
func (r *RBACManager) RemoveRole(userID, tenantID string) bool {
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
