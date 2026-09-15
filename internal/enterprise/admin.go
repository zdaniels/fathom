package enterprise

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"

	"github.com/zdaniels/fathom/internal/enterprise/core"
	"github.com/zdaniels/fathom/internal/security"
	"github.com/zdaniels/fathom/pkg/types"
)

// AdminAPI mounts /api/v1/admin/* endpoints on the gateway via its extension
// hook. RBAC-gated for every action, with optional defense-in-depth layers
// (IP allow-list, rate limit, step-up re-auth) configured via AdminConfig.
type AdminAPI struct {
	RBAC       *core.RBACManager
	Tenants    *core.TenantManager
	Compliance *core.Exporter
	// Audit accepts any security.Recorder so the same handler can be
	// reused by fathom-gateway's admin surface with its own audit sink.
	Audit security.Recorder

	// AuthFn translates an HTTP request to a userID; returns an error if the
	// request isn't authenticated. mountEnterprise passes a closure that calls
	// the gateway's bearer-token auth.
	AuthFn func(r *http.Request) (string, error)

	// --- belt-and-suspenders layers (all optional) ---

	// AllowNets, when non-empty, restricts admin endpoints to these source
	// IP ranges (checked against RemoteAddr). Empty = no IP restriction.
	AllowNets []*net.IPNet
	// Limiter caps admin requests per source IP. nil = unlimited.
	Limiter *adminRateLimiter
	// RequireStepUp gates destructive mutations behind a fresh re-auth.
	RequireStepUp bool
	// StepUpAuth re-authenticates the X-Step-Up-Token value and reports the
	// userID + whether it currently holds admin (ManageUsers). Required when
	// RequireStepUp is set.
	StepUpAuth func(token string) (userID string, isAdmin bool)
}

// Handle is the extension-hook entrypoint. Returns true if it handled the
// request (response written), false to let the gateway 404.
func (a *AdminAPI) Handle(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if path != "/api/v1/audit" && !startsWith(path, "/api/v1/admin") {
		return false
	}

	// Layer: IP allow-list. When configured, anything outside the allowed
	// ranges gets a 404 — the admin surface is invisible to off-network
	// callers rather than advertising that it exists.
	ip := clientIP(r)
	if len(a.AllowNets) > 0 && !ipAllowed(a.AllowNets, ip) {
		respJSON(w, http.StatusNotFound, map[string]string{"error": "Not found"})
		return true
	}

	// Layer: per-IP rate limit. Forecloses brute force / token abuse.
	if !a.Limiter.allow(ip) {
		respJSON(w, http.StatusTooManyRequests, map[string]string{"error": "Too many admin requests; slow down"})
		return true
	}

	userID, err := a.AuthFn(r)
	if err != nil {
		respJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return true
	}

	switch {
	case path == "/api/v1/admin/users" && r.Method == http.MethodGet:
		if !a.requireManageUsers(w, userID, "list_users") {
			return true
		}
		respJSON(w, http.StatusOK, map[string]interface{}{"users": a.RBAC.ListAssignments("")})

	case path == "/api/v1/admin/users/role" && r.Method == http.MethodPost:
		if !a.requireManageUsers(w, userID, "assign_role") {
			return true
		}
		var body struct {
			UserID   string `json:"userId"`
			Role     string `json:"role"`
			TenantID string `json:"tenantId,omitempty"`
		}
		if err := readJSON(r, &body); err != nil {
			respJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return true
		}
		detail := map[string]interface{}{"op": "assign_role", "target": body.UserID, "role": body.Role, "tenant": body.TenantID}
		// Destructive mutation → step-up gate.
		if !a.stepUpOK(r) {
			a.audit(userID, "assign_role", types.PolicyDeny, withReason(detail, "step_up_required"))
			respJSON(w, http.StatusForbidden, map[string]string{"error": "Step-up re-authentication required: resupply a valid admin token in X-Step-Up-Token"})
			return true
		}
		a.RBAC.AssignRole(body.UserID, core.Role(body.Role), userID, body.TenantID)
		a.audit(userID, "assign_role", types.PolicyAllow, detail)
		respJSON(w, http.StatusOK, map[string]bool{"success": true})

	case path == "/api/v1/admin/tenants" && r.Method == http.MethodGet:
		if !a.requireManageUsers(w, userID, "list_tenants") {
			return true
		}
		respJSON(w, http.StatusOK, map[string]interface{}{"tenants": a.Tenants.List()})

	case path == "/api/v1/admin/tenants" && r.Method == http.MethodPost:
		if !a.requireManageUsers(w, userID, "create_tenant") {
			return true
		}
		var body struct {
			Name   string             `json:"name"`
			Slug   string             `json:"slug"`
			Config *core.TenantConfig `json:"config,omitempty"`
		}
		if err := readJSON(r, &body); err != nil {
			respJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return true
		}
		detail := map[string]interface{}{"op": "create_tenant", "name": body.Name, "slug": body.Slug}
		if !a.stepUpOK(r) {
			a.audit(userID, "create_tenant", types.PolicyDeny, withReason(detail, "step_up_required"))
			respJSON(w, http.StatusForbidden, map[string]string{"error": "Step-up re-authentication required: resupply a valid admin token in X-Step-Up-Token"})
			return true
		}
		tt, err := a.Tenants.Create(body.Name, body.Slug, body.Config)
		if err != nil {
			respJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return true
		}
		a.audit(userID, "create_tenant", types.PolicyAllow, detail)
		respJSON(w, http.StatusCreated, map[string]interface{}{"tenant": tt})

	case path == "/api/v1/audit" && r.Method == http.MethodGet:
		canViewAll := a.RBAC.HasPermission(userID, "", func(p core.Permissions) bool { return p.ViewAuditLogs })
		entries := a.Audit.Snapshot()
		if !canViewAll {
			filtered := entries[:0]
			for _, e := range entries {
				if e.UserID == userID {
					filtered = append(filtered, e)
				}
			}
			entries = filtered
		}
		limit := parseIntDefault(r.URL.Query().Get("limit"), 100)
		if limit > len(entries) {
			limit = len(entries)
		}
		respJSON(w, http.StatusOK, map[string]interface{}{
			"entries": entries[len(entries)-limit:],
			"total":   len(entries),
		})

	case path == "/api/v1/admin/compliance" && r.Method == http.MethodPost:
		if !a.RBAC.HasPermission(userID, "", func(p core.Permissions) bool { return p.ViewAuditLogs }) {
			a.audit(userID, "compliance_export", types.PolicyDeny, map[string]interface{}{"reason": "permission_denied:view_audit_logs"})
			respJSON(w, http.StatusForbidden, map[string]string{"error": "Permission denied: view_audit_logs"})
			return true
		}
		var body struct {
			Framework string      `json:"framework"`
			Format    string      `json:"format"`
			Period    core.Period `json:"period"`
		}
		if err := readJSON(r, &body); err != nil {
			respJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return true
		}
		report := a.Compliance.Generate(core.Framework(body.Framework), a.Audit.Snapshot(), body.Period)
		if body.Format == "csv" {
			w.Header().Set("Content-Type", "text/csv")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(a.Compliance.ExportCSV(report)))
			return true
		}
		respJSON(w, http.StatusOK, report)

	default:
		respJSON(w, http.StatusNotFound, map[string]string{"error": "Admin endpoint not found"})
	}
	return true
}

// requireManageUsers enforces the ManageUsers permission and audits the
// denial when it fails. Returns true to proceed. Centralising it here means
// every admin mutation/list records *failed* attempts too, not just allowed
// ones — that's the forensic value of the audit suspender.
func (a *AdminAPI) requireManageUsers(w http.ResponseWriter, userID, op string) bool {
	if a.RBAC.HasPermission(userID, "", func(p core.Permissions) bool { return p.ManageUsers }) {
		return true
	}
	a.audit(userID, op, types.PolicyDeny, map[string]interface{}{"reason": "permission_denied:manage_users"})
	respJSON(w, http.StatusForbidden, map[string]string{"error": "Permission denied: manage_users"})
	return false
}

// stepUpOK reports whether the request satisfies the step-up requirement for
// a destructive mutation. When RequireStepUp is off it's always true. When
// on, the caller must present X-Step-Up-Token that re-authenticates to a
// user who currently holds admin — sudo-style proof of live possession,
// defeating a passively replayed session token.
func (a *AdminAPI) stepUpOK(r *http.Request) bool {
	if !a.RequireStepUp {
		return true
	}
	if a.StepUpAuth == nil {
		return false // configured to require step-up but no verifier wired → fail closed
	}
	tok := r.Header.Get("X-Step-Up-Token")
	if tok == "" {
		return false
	}
	_, isAdmin := a.StepUpAuth(tok)
	return isAdmin
}

// audit writes one admin-action entry (allowed or denied) when an audit sink
// is wired. No-op otherwise so the handler stays usable in tests/embedders
// that don't pass a recorder.
func (a *AdminAPI) audit(userID, op string, decision types.PolicyDecision, detail map[string]interface{}) {
	if a.Audit == nil {
		return
	}
	if detail == nil {
		detail = map[string]interface{}{}
	}
	detail["op"] = op
	_ = a.Audit.Log("", userID, types.AuditAdminAction, detail, decision)
}

func withReason(detail map[string]interface{}, reason string) map[string]interface{} {
	cp := make(map[string]interface{}, len(detail)+1)
	for k, v := range detail {
		cp[k] = v
	}
	cp["reason"] = reason
	return cp
}

func parseIntDefault(s string, d int) int {
	if s == "" {
		return d
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return d
	}
	return n
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func respJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func readJSON(r *http.Request, dst interface{}) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, dst)
}
