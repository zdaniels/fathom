package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zdaniels/fathom/internal/security"
	"github.com/zdaniels/fathom/pkg/types"
	"gopkg.in/yaml.v3"
)

// settingsTestGateway wires a gateway with a live policy engine + temp
// config/policy files so the settings handler can round-trip edits.
func settingsTestGateway(t *testing.T, mode types.Mode) (*Gateway, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	polPath := filepath.Join(dir, "policy.yaml")

	cfg := types.Config{
		Mode: mode,
		Host: "127.0.0.1", Port: 0,
		Auth: types.AuthConfig{SessionTimeout: time.Hour},
	}
	// Seed a minimal config + a deny-by-default policy on disk.
	_ = os.WriteFile(cfgPath, []byte("mode: "+string(mode)+"\nllm:\n  default: gpt-oss\n"), 0o644)
	startPol := types.PolicyConfig{
		Version: 1,
		Defaults: types.PermissionSet{
			Network: "deny", Filesystem: "read-only", Shell: "deny", Secrets: "isolated",
		},
	}
	buf, _ := yaml.Marshal(startPol)
	_ = os.WriteFile(polPath, buf, 0o644)

	g := New(cfg)
	tok, err := g.Auth.SetupInitialToken()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	engine := security.NewPolicyEngine(startPol)
	g.SetPolicyController(engine)
	g.SetSettingsPaths(cfgPath, polPath)
	g.SetModelRegistry(stubRegistry{names: []string{"gpt-oss", "hermes"}, def: "gpt-oss"})
	return g, tok, cfgPath, polPath
}

type stubRegistry struct {
	names []string
	def   string
}

func (s stubRegistry) Names() []string     { return s.names }
func (s stubRegistry) DefaultName() string { return s.def }

func doSettings(t *testing.T, g *Gateway, method, tok, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/api/v1/settings", nil)
	} else {
		r = httptest.NewRequest(method, "/api/v1/settings", strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer "+tok)
	r.RemoteAddr = "127.0.0.1:54321" // settings is loopback-only
	w := httptest.NewRecorder()
	g.handleSettings(w, r)
	return w
}

func TestSettingsGetPersonalEditable(t *testing.T) {
	g, tok, _, _ := settingsTestGateway(t, types.ModePersonal)
	w := doSettings(t, g, http.MethodGet, tok, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", w.Code, w.Body.String())
	}
	var v settingsView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !v.Editable {
		t.Error("personal mode should be editable")
	}
	if v.Features.Shell {
		t.Error("shell should be off (policy deny by default)")
	}
	if v.Policy.Network != "deny" {
		t.Errorf("network = %q want deny", v.Policy.Network)
	}
	if v.Models.Default != "gpt-oss" || len(v.Models.Available) != 2 {
		t.Errorf("models = %+v", v.Models)
	}
}

func TestSettingsRequiresAuth(t *testing.T) {
	g, _, _, _ := settingsTestGateway(t, types.ModePersonal)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	g.handleSettings(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no-auth = %d want 401", w.Code)
	}
}

// In PERSONAL mode settings must be invisible to non-loopback callers
// (relay/LAN), even with a valid token — the relay path leaves RemoteAddr
// empty, and any non-local peer is rejected with 404 before auth runs.
func TestSettingsPersonalRemoteCallerGets404(t *testing.T) {
	g, tok, _, _ := settingsTestGateway(t, types.ModePersonal)
	cases := []string{"", "203.0.113.7:443", "192.168.1.5:6000"} // relay (empty) + WAN + LAN
	for _, addr := range cases {
		for _, method := range []string{http.MethodGet, http.MethodPatch} {
			r := httptest.NewRequest(method, "/api/v1/settings", strings.NewReader(`{"features":{"shell":true}}`))
			r.Header.Set("Authorization", "Bearer "+tok)
			r.RemoteAddr = addr
			w := httptest.NewRecorder()
			g.handleSettings(w, r)
			if w.Code != http.StatusNotFound {
				t.Errorf("personal remote %s %s = %d, want 404", method, addr, w.Code)
			}
		}
	}
}

// In TEAM/ENTERPRISE mode the gateway runs on a server and admins connect
// over the network, so a remote (non-loopback) caller must reach settings —
// RBAC, not loopback, is the boundary. A remote viewer can GET (read-only)
// but PATCH is still 403; the loopback gate must not turn these into 404s.
func TestSettingsEnterpriseRemoteAllowed(t *testing.T) {
	g, tok, _, _ := settingsTestGateway(t, types.ModeEnterprise)

	get := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	get.Header.Set("Authorization", "Bearer "+tok)
	get.RemoteAddr = "203.0.113.7:443" // remote admin laptop
	gw := httptest.NewRecorder()
	g.handleSettings(gw, get)
	if gw.Code != http.StatusOK {
		t.Fatalf("enterprise remote GET = %d, want 200 (body=%s)", gw.Code, gw.Body.String())
	}

	// No edit-auth wired → enterprise defaults to read-only → PATCH 403 (NOT 404).
	patch := httptest.NewRequest(http.MethodPatch, "/api/v1/settings", strings.NewReader(`{"features":{"shell":true}}`))
	patch.Header.Set("Authorization", "Bearer "+tok)
	patch.RemoteAddr = "203.0.113.7:443"
	pw := httptest.NewRecorder()
	g.handleSettings(pw, patch)
	if pw.Code != http.StatusForbidden {
		t.Errorf("enterprise remote viewer PATCH = %d, want 403", pw.Code)
	}
}

func TestSettingsPatchShellTogglesLivePolicy(t *testing.T) {
	g, tok, _, polPath := settingsTestGateway(t, types.ModePersonal)

	// Before: shell-exec denied by default.
	ctl := g.policyCtl.(*security.PolicyEngine)
	if dec := ctl.Evaluate(security.PolicyContext{Action: "shell-exec", Command: "ls"}); dec.Decision != types.PolicyDeny {
		t.Fatalf("pre: shell-exec = %s want deny", dec.Decision)
	}

	w := doSettings(t, g, http.MethodPatch, tok, `{"features":{"shell":true}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d body=%s", w.Code, w.Body.String())
	}

	// After: engine hot-reloaded to allow shell-exec.
	if dec := ctl.Evaluate(security.PolicyContext{Action: "shell-exec", Command: "ls"}); dec.Decision != types.PolicyAllow {
		t.Errorf("post: shell-exec = %s want allow", dec.Decision)
	}
	// And persisted to disk.
	data, _ := os.ReadFile(polPath)
	var onDisk types.PolicyConfig
	_ = yaml.Unmarshal(data, &onDisk)
	if onDisk.Defaults.Shell != "allow" {
		t.Errorf("on-disk shell = %q want allow", onDisk.Defaults.Shell)
	}
}

func TestSettingsPatchWebSearchRuleRoundTrip(t *testing.T) {
	g, tok, _, _ := settingsTestGateway(t, types.ModePersonal)
	ctl := g.policyCtl.(*security.PolicyEngine)

	// Network is deny-by-default → web search blocked.
	if dec := ctl.Evaluate(security.PolicyContext{Action: "network", Domain: "duckduckgo.com"}); dec.Decision != types.PolicyDeny {
		t.Fatalf("pre: ddg = %s want deny", dec.Decision)
	}

	// Enable web search.
	if w := doSettings(t, g, http.MethodPatch, tok, `{"features":{"webSearch":true}}`); w.Code != http.StatusOK {
		t.Fatalf("enable status=%d body=%s", w.Code, w.Body.String())
	}
	if dec := ctl.Evaluate(security.PolicyContext{Action: "network", Domain: "duckduckgo.com"}); dec.Decision != types.PolicyAllow {
		t.Errorf("post-enable: ddg = %s want allow", dec.Decision)
	}

	// Disable again → rule removed, back to deny.
	if w := doSettings(t, g, http.MethodPatch, tok, `{"features":{"webSearch":false}}`); w.Code != http.StatusOK {
		t.Fatalf("disable status=%d", w.Code)
	}
	if dec := ctl.Evaluate(security.PolicyContext{Action: "network", Domain: "duckduckgo.com"}); dec.Decision != types.PolicyDeny {
		t.Errorf("post-disable: ddg = %s want deny", dec.Decision)
	}
}

func TestSettingsPatchModelDefaultRestartFlag(t *testing.T) {
	g, tok, cfgPath, _ := settingsTestGateway(t, types.ModePersonal)
	w := doSettings(t, g, http.MethodPatch, tok, `{"models":{"default":"hermes"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var v settingsView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if !v.RestartRequired {
		t.Error("model change should set restartRequired")
	}
	// Persisted under llm.default without clobbering the other keys.
	m := readConfigMap(cfgPath)
	llm, _ := m["llm"].(map[string]interface{})
	if llm == nil || llm["default"] != "hermes" {
		t.Errorf("on-disk llm.default = %v want hermes (full map: %v)", llm, m)
	}
}

func TestSettingsPatchSwarmToggle(t *testing.T) {
	g, tok, cfgPath, _ := settingsTestGateway(t, types.ModePersonal)

	// GET: swarm off by default (no swarm block in the seeded config).
	gw := doSettings(t, g, http.MethodGet, tok, "")
	var got settingsView
	_ = json.Unmarshal(gw.Body.Bytes(), &got)
	if got.Features.Swarm {
		t.Error("swarm should be off by default")
	}

	// PATCH: enable swarm → persisted to config.swarm.enabled + restart flag +
	// the response reflects it (read from disk, not the boot snapshot).
	w := doSettings(t, g, http.MethodPatch, tok, `{"features":{"swarm":true}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var v settingsView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if !v.RestartRequired {
		t.Error("swarm change should set restartRequired")
	}
	if !v.Features.Swarm {
		t.Error("PATCH response should report swarm enabled")
	}
	m := readConfigMap(cfgPath)
	sw, _ := m["swarm"].(map[string]interface{})
	if sw == nil || sw["enabled"] != true {
		t.Errorf("on-disk swarm.enabled = %v want true (full map: %v)", sw, m)
	}
}

func TestSettingsPatchClaudeCodeToggle(t *testing.T) {
	g, tok, cfgPath, _ := settingsTestGateway(t, types.ModePersonal)

	gw := doSettings(t, g, http.MethodGet, tok, "")
	var got settingsView
	_ = json.Unmarshal(gw.Body.Bytes(), &got)
	if got.Features.ClaudeCode {
		t.Error("claude-code should be off by default")
	}

	w := doSettings(t, g, http.MethodPatch, tok, `{"features":{"claudeCode":true}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var v settingsView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if !v.RestartRequired {
		t.Error("claude-code change should set restartRequired")
	}
	if !v.Features.ClaudeCode {
		t.Error("PATCH response should report claude-code enabled")
	}
	m := readConfigMap(cfgPath)
	cc, _ := m["claudeCode"].(map[string]interface{})
	if cc == nil || cc["enabled"] != true {
		t.Errorf("on-disk claudeCode.enabled = %v want true (full map: %v)", cc, m)
	}
}

func TestSettingsPatchTakeover(t *testing.T) {
	g, tok, cfgPath, _ := settingsTestGateway(t, types.ModePersonal)

	// Default: off, provider defaults to "claude", providers list populated.
	gw := doSettings(t, g, http.MethodGet, tok, "")
	var got settingsView
	_ = json.Unmarshal(gw.Body.Bytes(), &got)
	if got.Takeover.Enabled || got.Takeover.Provider != "claude" || len(got.Takeover.Providers) < 2 {
		t.Errorf("default takeover = %+v; want disabled, provider=claude, ≥2 providers", got.Takeover)
	}

	// Enable + pick codex → both persist to the takeover block + restart flag.
	w := doSettings(t, g, http.MethodPatch, tok, `{"takeover":{"enabled":true,"provider":"codex"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var v settingsView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if !v.RestartRequired || !v.Takeover.Enabled || v.Takeover.Provider != "codex" {
		t.Errorf("after PATCH: restart=%v enabled=%v provider=%q want true,true,codex", v.RestartRequired, v.Takeover.Enabled, v.Takeover.Provider)
	}
	m := readConfigMap(cfgPath)
	tk, _ := m["takeover"].(map[string]interface{})
	if tk == nil || tk["enabled"] != true || tk["provider"] != "codex" {
		t.Errorf("on-disk takeover = %v want {enabled:true, provider:codex} (full map: %v)", tk, m)
	}
}

func TestSettingsPolicyListsPersist(t *testing.T) {
	g, tok, _, polPath := settingsTestGateway(t, types.ModePersonal)
	body := `{"policy":{"network":"allow","allowedDomains":["api.github.com","*.openai.com"],"deniedPaths":["**/.ssh/**"]}}`
	if w := doSettings(t, g, http.MethodPatch, tok, body); w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	data, _ := os.ReadFile(polPath)
	var onDisk types.PolicyConfig
	_ = yaml.Unmarshal(data, &onDisk)
	if onDisk.Defaults.Network != "allow" {
		t.Errorf("network = %q", onDisk.Defaults.Network)
	}
	if len(onDisk.Defaults.AllowedDomains) != 2 || onDisk.Defaults.AllowedDomains[0] != "api.github.com" {
		t.Errorf("allowedDomains = %v", onDisk.Defaults.AllowedDomains)
	}
	if len(onDisk.Defaults.DeniedPaths) != 1 {
		t.Errorf("deniedPaths = %v", onDisk.Defaults.DeniedPaths)
	}
}

func TestSettingsTeamViewerReadOnly(t *testing.T) {
	g, tok, _, _ := settingsTestGateway(t, types.ModeTeam)
	// No edit-auth wired → team mode defaults to read-only.
	w := doSettings(t, g, http.MethodGet, tok, "")
	var v settingsView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if v.Editable {
		t.Error("team mode without RBAC wiring should be read-only")
	}
	// PATCH must be rejected.
	pw := doSettings(t, g, http.MethodPatch, tok, `{"features":{"shell":true}}`)
	if pw.Code != http.StatusForbidden {
		t.Errorf("team viewer PATCH = %d want 403", pw.Code)
	}
}

func TestSettingsTeamAdminCanEdit(t *testing.T) {
	g, tok, _, _ := settingsTestGateway(t, types.ModeTeam)
	g.SetSettingsEditAuth(func(string) bool { return true }) // simulate RBAC admin
	w := doSettings(t, g, http.MethodPatch, tok, `{"features":{"shell":true}}`)
	if w.Code != http.StatusOK {
		t.Errorf("team admin PATCH = %d want 200 body=%s", w.Code, w.Body.String())
	}
}
