package enterprise

import (
	"errors"
	"github.com/zdaniels/fathom/internal/brandenv"
	"log/slog"
	"net/http"
	"strings"

	"github.com/zdaniels/fathom/internal/auth"
	"github.com/zdaniels/fathom/internal/enterprise/core"
	"github.com/zdaniels/fathom/internal/gateway"
	"github.com/zdaniels/fathom/internal/security"
	"github.com/zdaniels/fathom/pkg/types"
)

// Mounted is what Mount returns when the caller wants references to the
// individual managers (e.g. for boot banner output).
type Mounted struct {
	RBAC       *core.RBACManager
	Tenants    *core.TenantManager
	Compliance *core.Exporter
	Admin      *AdminAPI
	SSO        *SSOManager
}

// Mount wires the enterprise stack onto a running Gateway. In personal mode
// nothing happens — returns (nil, nil). In team/enterprise mode it:
//  1. Creates RBAC + tenants + compliance + (enterprise only) SSO.
//  2. Bootstrap-assigns the "admin" userID the admin role (override via
//     FANTAZM_BOOTSTRAP_ADMIN). Without this, the auto-generated initial
//     token's owner would default to viewer and every /api/v1/admin/* call
//     would return 403.
//  3. Wires the AdminAPI onto Gateway.SetExtensionHandler so /api/v1/admin/*
//     and /api/v1/audit get routed.
func Mount(gw *gateway.Gateway, cfg types.Config, mesh *security.Mesh) (*Mounted, error) {
	if cfg.Mode == types.ModePersonal {
		return nil, nil
	}
	if gw == nil || mesh == nil {
		return nil, errors.New("Mount requires a gateway and security mesh")
	}

	rbac := core.NewRBACManager()
	tenants := core.NewTenantManager()
	compliance := core.NewExporter()

	bootstrap := brandenv.Get("FATHOM_BOOTSTRAP_ADMIN")
	if bootstrap == "" {
		bootstrap = "admin"
	}
	rbac.AssignRole(bootstrap, core.RoleAdmin, "bootstrap", "")
	slog.Info("bootstrap admin role assigned", "userId", bootstrap)

	authFn := func(r *http.Request) (string, error) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			return "", errors.New("Missing or invalid Authorization header")
		}
		res, err := gw.Auth.Authenticate(strings.TrimPrefix(h, "Bearer "))
		if err != nil {
			return "", err
		}
		return res.UserID, nil
	}

	admin := &AdminAPI{
		RBAC:       rbac,
		Tenants:    tenants,
		Compliance: compliance,
		Audit:      mesh.Audit,
		AuthFn:     authFn,
	}

	// Belt-and-suspenders hardening for the admin surface, from
	// cfg.Enterprise.Admin (+ FANTAZM_ADMIN_ALLOW_CIDRS for the allow-list).
	rateLimit := 60 // default: 60 admin req/IP/min
	if ac := adminCfg(cfg); ac != nil {
		cidrs := ac.AllowCIDRs
		if env := brandenv.Get("FATHOM_ADMIN_ALLOW_CIDRS"); env != "" {
			cidrs = append(cidrs, strings.Split(env, ",")...)
		}
		admin.AllowNets = parseCIDRs(cidrs)
		if ac.RateLimitPerMin != 0 {
			rateLimit = ac.RateLimitPerMin // negative disables
		}
		admin.RequireStepUp = ac.RequireStepUp
	} else if env := brandenv.Get("FATHOM_ADMIN_ALLOW_CIDRS"); env != "" {
		admin.AllowNets = parseCIDRs(strings.Split(env, ","))
	}
	admin.Limiter = newAdminRateLimiter(rateLimit)
	// Step-up verifier: re-authenticate the supplied token and confirm it
	// still holds admin (ManageUsers) right now.
	admin.StepUpAuth = func(token string) (string, bool) {
		res, err := gw.Auth.Authenticate(token)
		if err != nil {
			return "", false
		}
		isAdmin := rbac.HasPermission(res.UserID, "", func(p core.Permissions) bool { return p.ManageUsers })
		return res.UserID, isAdmin
	}
	if admin.RequireStepUp || len(admin.AllowNets) > 0 {
		slog.Info("admin surface hardening enabled",
			"ipAllowList", len(admin.AllowNets) > 0,
			"rateLimitPerMin", rateLimit,
			"requireStepUp", admin.RequireStepUp,
		)
	}

	gw.SetExtensionHandler(admin.Handle)

	// Settings page write-gate: in team/enterprise mode only admins (the
	// ManagePolicies permission) may modify settings. Everyone else gets a
	// read-only view — the gateway's settings handler renders the controls
	// disabled when this predicate returns false.
	gw.SetSettingsEditAuth(func(userID string) bool {
		return rbac.HasPermission(userID, "", func(p core.Permissions) bool { return p.ManagePolicies })
	})

	mounted := &Mounted{RBAC: rbac, Tenants: tenants, Compliance: compliance, Admin: admin}
	if cfg.Mode == types.ModeEnterprise && cfg.Enterprise != nil && cfg.Enterprise.SSO != nil {
		sso := NewSSOManager()
		redirect := "http://" + cfg.Host + ":" + portStr(cfg.Port) + "/api/v1/sso/callback"
		sso.Configure(OIDCConfig{
			Issuer:       cfg.Enterprise.SSO.Issuer,
			ClientID:     cfg.Enterprise.SSO.ClientID,
			ClientSecret: cfg.Enterprise.SSO.ClientSecret,
			RedirectURI:  redirect,
		})
		mounted.SSO = sso
		slog.Info("SSO/OIDC enabled", "issuer", cfg.Enterprise.SSO.Issuer)
	}
	slog.Info("enterprise features mounted",
		"mode", cfg.Mode,
		"sso", mounted.SSO != nil,
		"routes", "/api/v1/admin/*, /api/v1/audit",
	)
	return mounted, nil
}

// adminCfg pulls the optional admin-hardening block out of the config,
// nil-safe through the enterprise block.
func adminCfg(cfg types.Config) *types.AdminConfig {
	if cfg.Enterprise == nil {
		return nil
	}
	return cfg.Enterprise.Admin
}

func portStr(p int) string {
	if p == 0 {
		return "8790"
	}
	var b [16]byte
	i := len(b)
	for p > 0 {
		i--
		b[i] = byte('0' + p%10)
		p /= 10
	}
	return string(b[i:])
}

// Reference the auth package so the import stays even when builds skip its
// type-only uses.
var _ = auth.Manager{}
