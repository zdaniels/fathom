package enterprise

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zdaniels/fathom/internal/enterprise/core"
	"github.com/zdaniels/fathom/internal/gateway"
	"github.com/zdaniels/fathom/internal/security"
	"github.com/zdaniels/fathom/pkg/types"
)

func TestCIDRValidation(t *testing.T) {
	for _, entry := range []string{"", "not-a-cidr", "10.0.0.0/99", ","} {
		if _, err := parseCIDRs([]string{entry}); err == nil {
			t.Fatalf("accepted %q", entry)
		}
	}
	nets, err := parseCIDRs([]string{" 127.0.0.1 ", " ::1/128 "})
	if err != nil || !ipAllowed(nets, "127.0.0.1") || !ipAllowed(nets, "::1") {
		t.Fatalf("whitespace/IP parsing: %v", err)
	}
}
func TestAuditPaginationRejectsInvalidInput(t *testing.T) {
	a := adminWith("root", nil)
	for _, raw := range []string{"-1", "abc", "9999999999999999999999", "10001", ""} {
		w := httptest.NewRecorder()
		a.Handle(w, req("GET", "/api/v1/audit?limit="+raw, "", "127.0.0.1:123"))
		if w.Code != 400 {
			t.Fatalf("limit=%q status %d", raw, w.Code)
		}
	}
	for _, raw := range []string{"0", "1", "10000"} {
		w := httptest.NewRecorder()
		a.Handle(w, req("GET", "/api/v1/audit?limit="+raw, "", "127.0.0.1:123"))
		if w.Code != 200 {
			t.Fatalf("limit=%q status %d", raw, w.Code)
		}
	}
}
func TestSettingsUsesAdminHardeningAndViewerCannotDispatch(t *testing.T) {
	cfg := types.DefaultConfig()
	cfg.Mode = types.ModeTeam
	cfg.Port = 0
	cfg.DataDir = t.TempDir()
	cfg.Enterprise = &types.EnterpriseBlock{Admin: &types.AdminConfig{AllowCIDRs: []string{"127.0.0.1/32"}}}
	g := gateway.New(cfg)
	adminToken, _ := g.Auth.CreateAPIToken("admin", "test")
	viewerToken, _ := g.Auth.CreateAPIToken("viewer", "test")
	mesh := security.NewMesh(types.PolicyConfig{}, security.MeshOptions{})
	ent, err := Mount(g, cfg, mesh)
	if err != nil {
		t.Fatal(err)
	}
	defer ent.DB.Close()
	g.SetMessageHandler(func(context.Context, types.ChannelMessage, types.Session) (string, error) {
		t.Error("unauthorized dispatch")
		return "", nil
	})
	if err := g.Start(); err != nil {
		t.Fatal(err)
	}
	defer g.Stop(context.Background())
	request := func(method, path, token, remote string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(`{"text":"run","features":{"subAgents":true}}`))
		r.RemoteAddr = remote
		r.Header.Set("Authorization", "Bearer "+token)
		g.Handler().ServeHTTP(w, r)
		return w.Code
	}
	if got := request("PATCH", "/api/v1/settings", adminToken, "203.0.113.1:123"); got != 404 {
		t.Fatalf("off-network settings: %d", got)
	}
	ent.Admin.RequireStepUp = true
	if got := request("PATCH", "/api/v1/settings", adminToken, "127.0.0.1:123"); got != 403 {
		t.Fatalf("no step-up: %d", got)
	}
	for _, path := range []string{"/api/v1/message", "/api/v1/stream"} {
		if got := request("POST", path, viewerToken, "127.0.0.1:123"); got != 403 {
			t.Fatalf("viewer %s: %d", path, got)
		}
	}
	tenant, err := ent.Tenants.Create("Other tenant", "other", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ent.RBAC.AssignRole("viewer", core.RoleAdmin, "admin", tenant.ID); err != nil {
		t.Fatal(err)
	}
	if got := request("POST", "/api/v1/message", viewerToken, "127.0.0.1:123"); got != 403 {
		t.Fatalf("tenant role admitted to shared instance: %d", got)
	}
}
func TestStepUpMustBelongToSameUser(t *testing.T) {
	a := adminWith("alice", func(a *AdminAPI) {
		a.RequireStepUp = true
		a.StepUpAuth = func(string) (string, bool) { return "bob", true }
	})
	r := req("POST", "/api/v1/admin/users/role", `{"userId":"x","role":"operator"}`, "")
	r.Header.Set("X-Step-Up-Token", "bob-fresh")
	w := httptest.NewRecorder()
	a.Handle(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal(w.Code)
	}
}

func TestRoleRevocationReachesToolPolicy(t *testing.T) {
	cfg := types.DefaultConfig()
	cfg.Mode = types.ModeTeam
	cfg.DataDir = t.TempDir()
	g := gateway.New(cfg)
	mesh := security.NewMesh(types.PolicyConfig{}, security.MeshOptions{})
	ent, err := Mount(g, cfg, mesh)
	if err != nil {
		t.Fatal(err)
	}
	defer ent.DB.Close()
	ent.RBAC.AssignRole("alice", core.RoleOperator, "admin", "")
	if !mesh.Policy.UserAllowed("alice") {
		t.Fatal("operator denied")
	}
	ent.RBAC.AssignRole("alice", core.RoleViewer, "admin", "")
	got := mesh.Policy.Evaluate(security.PolicyContext{UserID: "alice", Action: "tool:read_file"})
	if got.Decision != types.PolicyDeny {
		t.Fatal("revoked user could execute tool")
	}
}
