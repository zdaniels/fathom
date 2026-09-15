package enterprise

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zdaniels/fathom/internal/enterprise/core"
	"github.com/zdaniels/fathom/pkg/types"
)

// adminWith returns a test admin whose authUser holds the admin role, plus
// any per-test field tweaks applied by fn.
func adminWith(authUser string, fn func(*AdminAPI)) *AdminAPI {
	a := newTestAdmin(authUser)
	a.RBAC.AssignRole(authUser, core.RoleAdmin, "boot", "")
	if fn != nil {
		fn(a)
	}
	return a
}

func req(method, path, body, remote string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if remote != "" {
		r.RemoteAddr = remote
	}
	return r
}

func countAdminAudit(a *AdminAPI, decision types.PolicyDecision) int {
	n := 0
	for _, e := range a.Audit.Snapshot() {
		if e.Action == types.AuditAdminAction && e.PolicyResult == decision {
			n++
		}
	}
	return n
}

// --- Layer 1: audit mutations + denials ---

func TestAdminMutationIsAudited(t *testing.T) {
	a := adminWith("root", nil)
	w := httptest.NewRecorder()
	a.Handle(w, req(http.MethodPost, "/api/v1/admin/users/role", `{"userId":"new","role":"operator"}`, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if countAdminAudit(a, types.PolicyAllow) != 1 {
		t.Errorf("expected 1 allowed admin_action audit entry, got %d", countAdminAudit(a, types.PolicyAllow))
	}
}

func TestAdminDeniedAttemptIsAudited(t *testing.T) {
	a := newTestAdmin("bob")
	a.RBAC.AssignRole("bob", core.RoleOperator, "boot", "") // not allowed to manage users
	w := httptest.NewRecorder()
	a.Handle(w, req(http.MethodPost, "/api/v1/admin/users/role", `{"userId":"x","role":"admin"}`, ""))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403", w.Code)
	}
	if countAdminAudit(a, types.PolicyDeny) != 1 {
		t.Errorf("expected 1 denied admin_action audit entry, got %d", countAdminAudit(a, types.PolicyDeny))
	}
}

// --- Layer 2: IP allow-list ---

func TestAdminIPAllowListBlocksOutsiders(t *testing.T) {
	a := adminWith("root", func(a *AdminAPI) {
		a.AllowNets = parseCIDRs([]string{"10.0.0.0/8"})
	})
	// Outside the allow-list → invisible (404), even though authed admin.
	w := httptest.NewRecorder()
	a.Handle(w, req(http.MethodGet, "/api/v1/admin/users", "", "192.0.2.5:4000"))
	if w.Code != http.StatusNotFound {
		t.Errorf("outsider = %d, want 404", w.Code)
	}
	// Inside the allow-list → proceeds (200).
	w2 := httptest.NewRecorder()
	a.Handle(w2, req(http.MethodGet, "/api/v1/admin/users", "", "10.1.2.3:4000"))
	if w2.Code != http.StatusOK {
		t.Errorf("insider = %d, want 200 (body=%s)", w2.Code, w2.Body.String())
	}
}

// --- Layer 3: rate limit ---

func TestAdminRateLimit(t *testing.T) {
	a := adminWith("root", func(a *AdminAPI) {
		a.Limiter = newAdminRateLimiter(2)
	})
	codes := []int{}
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		a.Handle(w, req(http.MethodGet, "/api/v1/admin/users", "", "203.0.113.9:5000"))
		codes = append(codes, w.Code)
	}
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
		t.Errorf("first two requests = %v, want 200,200", codes[:2])
	}
	if codes[2] != http.StatusTooManyRequests {
		t.Errorf("third request = %d, want 429", codes[2])
	}
}

// --- Layer 4: step-up for destructive ops ---

func TestAdminStepUpRequiredForMutation(t *testing.T) {
	a := adminWith("root", func(a *AdminAPI) {
		a.RequireStepUp = true
		a.StepUpAuth = func(tok string) (string, bool) { return "root", tok == "fresh-admin-token" }
	})

	// No step-up header → 403, and the denial is audited.
	w := httptest.NewRecorder()
	a.Handle(w, req(http.MethodPost, "/api/v1/admin/users/role", `{"userId":"new","role":"operator"}`, ""))
	if w.Code != http.StatusForbidden {
		t.Fatalf("no step-up = %d, want 403 (body=%s)", w.Code, w.Body.String())
	}
	if a.RBAC.GetRole("new", "") == core.RoleOperator {
		t.Error("role must NOT have been assigned without step-up")
	}
	if countAdminAudit(a, types.PolicyDeny) != 1 {
		t.Errorf("expected denied audit for missing step-up, got %d", countAdminAudit(a, types.PolicyDeny))
	}

	// Valid step-up token → succeeds.
	w2 := httptest.NewRecorder()
	r := req(http.MethodPost, "/api/v1/admin/users/role", `{"userId":"new","role":"operator"}`, "")
	r.Header.Set("X-Step-Up-Token", "fresh-admin-token")
	a.Handle(w2, r)
	if w2.Code != http.StatusOK {
		t.Fatalf("with step-up = %d, want 200 (body=%s)", w2.Code, w2.Body.String())
	}
	if a.RBAC.GetRole("new", "") != core.RoleOperator {
		t.Error("role should have been assigned after valid step-up")
	}
}

func TestAdminStepUpFailsClosedWithoutVerifier(t *testing.T) {
	a := adminWith("root", func(a *AdminAPI) {
		a.RequireStepUp = true
		a.StepUpAuth = nil // misconfigured — must fail closed, not open
	})
	w := httptest.NewRecorder()
	r := req(http.MethodPost, "/api/v1/admin/users/role", `{"userId":"new","role":"operator"}`, "")
	r.Header.Set("X-Step-Up-Token", "anything")
	a.Handle(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("missing verifier = %d, want 403 (fail closed)", w.Code)
	}
}
