package enterprise

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zdaniels/fathom/internal/enterprise/core"
	"github.com/zdaniels/fathom/internal/security"
)

func newTestAdmin(authUser string) *AdminAPI {
	return &AdminAPI{
		RBAC:       core.NewRBACManager(),
		Tenants:    core.NewTenantManager(),
		Compliance: core.NewExporter(),
		Audit:      security.NewAuditLogger(100),
		AuthFn: func(r *http.Request) (string, error) {
			if authUser == "" {
				return "", errors.New("unauthorized")
			}
			return authUser, nil
		},
	}
}

func TestAdminIgnoresUnrelatedPaths(t *testing.T) {
	a := newTestAdmin("anyone")
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	if a.Handle(w, r) {
		t.Error("admin should NOT handle /api/v1/health")
	}
}

func TestAdminUnauthorizedWhenAuthFnFails(t *testing.T) {
	a := newTestAdmin("")
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/users", nil)
	if !a.Handle(w, r) {
		t.Fatal("admin should claim the route")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestAdminListUsersRequiresManageUsersPermission(t *testing.T) {
	a := newTestAdmin("bob")
	a.RBAC.AssignRole("bob", core.RoleOperator, "boot", "") // operator can't manage users
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/users", nil)
	a.Handle(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("operator GET /users = %d, want 403", w.Code)
	}
}

func TestAdminListUsersOkForAdmin(t *testing.T) {
	a := newTestAdmin("root")
	a.RBAC.AssignRole("root", core.RoleAdmin, "boot", "")
	a.RBAC.AssignRole("alice", core.RoleViewer, "root", "")
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/users", nil)
	a.Handle(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	users, _ := body["users"].([]interface{})
	if len(users) < 2 {
		t.Errorf("expected ≥2 assignments, got %d", len(users))
	}
}

func TestAdminAssignRolePersistsThroughHTTP(t *testing.T) {
	a := newTestAdmin("root")
	a.RBAC.AssignRole("root", core.RoleAdmin, "boot", "")
	body := strings.NewReader(`{"userId":"new","role":"operator"}`)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users/role", body)
	a.Handle(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if got := a.RBAC.GetRole("new", ""); got != core.RoleOperator {
		t.Errorf("after POST: role = %v, want operator", got)
	}
}

func TestAdminCreateTenantConflict(t *testing.T) {
	a := newTestAdmin("root")
	a.RBAC.AssignRole("root", core.RoleAdmin, "boot", "")
	_, _ = a.Tenants.Create("Acme", "acme", nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/admin/tenants",
		strings.NewReader(`{"name":"Acme2","slug":"acme"}`))
	a.Handle(w, r)
	if w.Code != http.StatusConflict {
		t.Errorf("duplicate slug = %d, want 409", w.Code)
	}
}

func TestAdminAuditFiltersForNonAdmin(t *testing.T) {
	a := newTestAdmin("bob")
	a.RBAC.AssignRole("bob", core.RoleOperator, "boot", "")
	// Seed audit with entries for bob and someone else.
	a.Audit.Log("sess1", "bob", "tool_call", map[string]interface{}{"x": 1}, "allow")
	a.Audit.Log("sess2", "eve", "tool_call", map[string]interface{}{"x": 2}, "allow")
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/audit?limit=50", nil)
	a.Handle(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	entries, _ := body["entries"].([]interface{})
	for _, e := range entries {
		m, _ := e.(map[string]interface{})
		if m["userId"] != "bob" {
			t.Errorf("operator audit included entry for userId=%v (should be self-only)", m["userId"])
		}
	}
}

func TestAdminUnknownAdminPath(t *testing.T) {
	a := newTestAdmin("root")
	a.RBAC.AssignRole("root", core.RoleAdmin, "boot", "")
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/no-such-route", nil)
	if !a.Handle(w, r) {
		t.Fatal("admin should claim /api/v1/admin/*")
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}
