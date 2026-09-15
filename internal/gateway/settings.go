package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"

	"github.com/zdaniels/fathom/pkg/types"
	"gopkg.in/yaml.v3"
)

// errNoSettingsPath is returned when an edit is attempted but no config /
// policy path was wired (e.g. the gateway booted on in-memory defaults).
var errNoSettingsPath = errors.New("no settings file path configured (gateway booted without a config/policy file)")

// webSearchRuleName is the policy rule the "Web search" toggle manages.
// Network is deny-by-default, so the web-search tool (which checks
// CheckNetwork("duckduckgo.com") before its HTTP calls) is blocked until
// this allow rule exists. Toggling the feature adds/removes exactly this
// rule, leaving every other rule untouched.
const webSearchRuleName = "allow-web-search"

// webSearchDomains are the hosts the DuckDuckGo-backed web-search tool
// reaches. The tool gates on "duckduckgo.com"; the subdomains are listed
// for completeness so a future stricter per-host check still passes.
var webSearchDomains = []string{"duckduckgo.com", "*.duckduckgo.com"}

// settingsView is the JSON the settings page renders. `editable` tells the
// UI whether to enable the controls or show them read-only ("managed by
// your administrator").
type settingsView struct {
	Mode     string       `json:"mode"`
	Editable bool         `json:"editable"`
	Features featuresView `json:"features"`
	Models   modelsView   `json:"models"`
	Takeover takeoverView `json:"takeover"`
	Policy   policyView   `json:"policy"`
	// RestartRequired is set on PATCH responses when a saved change (model
	// default, sub-agents) only takes effect after a gateway restart.
	RestartRequired bool `json:"restartRequired,omitempty"`
}

type featuresView struct {
	Shell      bool `json:"shell"`      // policy defaults.shell == "allow"
	SubAgents  bool `json:"subAgents"`  // config subAgents.enabled
	Swarm      bool `json:"swarm"`      // config swarm.enabled
	ClaudeCode bool `json:"claudeCode"` // config claudeCode.enabled
	WebSearch  bool `json:"webSearch"`  // allow-web-search policy rule present
}

// takeoverViewProviders are the selectable takeover providers for the UI.
var takeoverViewProviders = []string{"claude", "codex"}

// takeoverView is the takeover section: route all traffic to an external
// coding-agent CLI, with a provider selector.
type takeoverView struct {
	Enabled   bool     `json:"enabled"`
	Provider  string   `json:"provider"`
	Providers []string `json:"providers"`
}

type modelsView struct {
	Default   string   `json:"default"`
	Available []string `json:"available"`
}

type policyView struct {
	Network        string   `json:"network"`        // "allow" | "deny"
	Filesystem     string   `json:"filesystem"`     // "read-only" | "read-write" | "deny"
	Secrets        string   `json:"secrets"`        // "isolated" | "accessible"
	AllowedDomains []string `json:"allowedDomains"` // editable allow-list
	AllowedPaths   []string `json:"allowedPaths"`
	DeniedPaths    []string `json:"deniedPaths"`
}

// settingsPatch is the partial-update body. Every field is a pointer so an
// absent key means "leave unchanged" — the UI can PATCH one toggle without
// resending the whole document.
type settingsPatch struct {
	Features *struct {
		Shell      *bool `json:"shell"`
		SubAgents  *bool `json:"subAgents"`
		Swarm      *bool `json:"swarm"`
		ClaudeCode *bool `json:"claudeCode"`
		WebSearch  *bool `json:"webSearch"`
	} `json:"features"`
	Models *struct {
		Default *string `json:"default"`
	} `json:"models"`
	Takeover *struct {
		Enabled  *bool   `json:"enabled"`
		Provider *string `json:"provider"`
	} `json:"takeover"`
	Policy *struct {
		Network        *string   `json:"network"`
		Filesystem     *string   `json:"filesystem"`
		Secrets        *string   `json:"secrets"`
		AllowedDomains *[]string `json:"allowedDomains"`
		AllowedPaths   *[]string `json:"allowedPaths"`
		DeniedPaths    *[]string `json:"deniedPaths"`
	} `json:"policy"`
}

func (g *Gateway) handleSettings(w http.ResponseWriter, r *http.Request) {
	// Reachability gate. The settings API can enable shell and rewrite the
	// security policy. In personal mode the gateway runs on the user's own
	// machine, so this is restricted to same-machine (loopback) callers —
	// never the relay or LAN. In team/enterprise mode the gateway runs on a
	// server and admins connect over the network, so remote is allowed and
	// RBAC (below) is the boundary. Non-reachable callers get 404 (not 403)
	// so the endpoint stays invisible rather than advertising it exists.
	if !g.settingsReachable(r) {
		jsonError(w, http.StatusNotFound, "Not found")
		return
	}
	authResult, ok := g.authenticate(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		g.serveSettingsGet(w, authResult.UserID)
	case http.MethodPatch, http.MethodPost:
		g.serveSettingsPatch(w, r, authResult.UserID)
	default:
		jsonError(w, http.StatusMethodNotAllowed, "GET or PATCH only")
	}
}

func (g *Gateway) serveSettingsGet(w http.ResponseWriter, userID string) {
	pol := g.currentPolicy()
	view := settingsView{
		Mode:     string(g.cfg.Mode),
		Editable: g.canEditSettings(userID),
		Features: featuresView{
			Shell:      pol.Defaults.Shell == "allow",
			SubAgents:  g.cfg.SubAgents != nil && g.cfg.SubAgents.Enabled,
			Swarm:      g.cfg.Swarm != nil && g.cfg.Swarm.Enabled,
			ClaudeCode: g.cfg.ClaudeCode != nil && g.cfg.ClaudeCode.Enabled,
			WebSearch:  hasRule(pol, webSearchRuleName),
		},
		Models:   modelsView{Default: g.modelDefault(), Available: g.modelNames()},
		Takeover: g.takeoverView(),
		Policy: policyView{
			Network:        pol.Defaults.Network,
			Filesystem:     pol.Defaults.Filesystem,
			Secrets:        pol.Defaults.Secrets,
			AllowedDomains: pol.Defaults.AllowedDomains,
			AllowedPaths:   pol.Defaults.AllowedPaths,
			DeniedPaths:    pol.Defaults.DeniedPaths,
		},
	}
	writeJSON(w, http.StatusOK, view)
}

func (g *Gateway) serveSettingsPatch(w http.ResponseWriter, r *http.Request, userID string) {
	if !g.canEditSettings(userID) {
		jsonError(w, http.StatusForbidden, "Settings are managed by your administrator and cannot be modified from this account")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 256*1024))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "body unreadable")
		return
	}
	var patch settingsPatch
	if err := json.Unmarshal(body, &patch); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// --- Policy changes (live-reloadable) ---
	pol := g.currentPolicy()
	policyTouched := false
	if patch.Features != nil {
		if patch.Features.Shell != nil {
			pol.Defaults.Shell = boolToMode(*patch.Features.Shell, "allow", "deny")
			policyTouched = true
		}
		if patch.Features.WebSearch != nil {
			if *patch.Features.WebSearch {
				pol = ensureWebSearchRule(pol)
			} else {
				pol = removeRule(pol, webSearchRuleName)
			}
			policyTouched = true
		}
	}
	if patch.Policy != nil {
		p := patch.Policy
		if p.Network != nil {
			pol.Defaults.Network = *p.Network
		}
		if p.Filesystem != nil {
			pol.Defaults.Filesystem = *p.Filesystem
		}
		if p.Secrets != nil {
			pol.Defaults.Secrets = *p.Secrets
		}
		if p.AllowedDomains != nil {
			pol.Defaults.AllowedDomains = *p.AllowedDomains
		}
		if p.AllowedPaths != nil {
			pol.Defaults.AllowedPaths = *p.AllowedPaths
		}
		if p.DeniedPaths != nil {
			pol.Defaults.DeniedPaths = *p.DeniedPaths
		}
		policyTouched = true
	}
	if policyTouched {
		if err := g.savePolicy(pol); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to persist policy: "+err.Error())
			return
		}
		// Hot-swap the live engine so policy changes apply without restart.
		g.mu.RLock()
		ctl := g.policyCtl
		g.mu.RUnlock()
		if ctl != nil {
			ctl.Reload(pol)
		}
	}

	// --- Config changes (require restart to take effect) ---
	restartRequired := false
	if patch.Models != nil && patch.Models.Default != nil {
		if err := g.saveConfigKey([]string{"llm", "default"}, *patch.Models.Default); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to persist model default: "+err.Error())
			return
		}
		restartRequired = true
	}
	if patch.Features != nil && patch.Features.SubAgents != nil {
		if err := g.saveConfigKey([]string{"subAgents", "enabled"}, *patch.Features.SubAgents); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to persist sub-agents toggle: "+err.Error())
			return
		}
		restartRequired = true
	}
	if patch.Features != nil && patch.Features.Swarm != nil {
		if err := g.saveConfigKey([]string{"swarm", "enabled"}, *patch.Features.Swarm); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to persist swarm toggle: "+err.Error())
			return
		}
		restartRequired = true
	}
	if patch.Features != nil && patch.Features.ClaudeCode != nil {
		if err := g.saveConfigKey([]string{"claudeCode", "enabled"}, *patch.Features.ClaudeCode); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to persist claude-code toggle: "+err.Error())
			return
		}
		restartRequired = true
	}
	if patch.Takeover != nil {
		if patch.Takeover.Enabled != nil {
			if err := g.saveConfigKey([]string{"takeover", "enabled"}, *patch.Takeover.Enabled); err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to persist takeover toggle: "+err.Error())
				return
			}
			restartRequired = true
		}
		if patch.Takeover.Provider != nil {
			if err := g.saveConfigKey([]string{"takeover", "provider"}, *patch.Takeover.Provider); err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to persist takeover provider: "+err.Error())
				return
			}
			restartRequired = true
		}
	}

	g.auditSettingsChange(userID, body)

	// Re-read and return the new effective state so the UI re-renders from truth.
	pol = g.currentPolicy()
	view := settingsView{
		Mode:     string(g.cfg.Mode),
		Editable: true,
		Features: featuresView{
			Shell:      pol.Defaults.Shell == "allow",
			SubAgents:  g.subAgentsEnabledOnDisk(),
			Swarm:      g.swarmEnabledOnDisk(),
			ClaudeCode: g.claudeCodeEnabledOnDisk(),
			WebSearch:  hasRule(pol, webSearchRuleName),
		},
		Models:          modelsView{Default: g.modelDefault(), Available: g.modelNames()},
		Takeover:        g.takeoverViewFromDisk(),
		RestartRequired: restartRequired,
		Policy: policyView{
			Network:        pol.Defaults.Network,
			Filesystem:     pol.Defaults.Filesystem,
			Secrets:        pol.Defaults.Secrets,
			AllowedDomains: pol.Defaults.AllowedDomains,
			AllowedPaths:   pol.Defaults.AllowedPaths,
			DeniedPaths:    pol.Defaults.DeniedPaths,
		},
	}
	writeJSON(w, http.StatusOK, view)
}

// currentPolicy returns the live policy if a controller is wired, otherwise
// reads the policy file from disk (falling back to deny-all defaults).
func (g *Gateway) currentPolicy() types.PolicyConfig {
	g.mu.RLock()
	ctl := g.policyCtl
	path := g.policyPath
	g.mu.RUnlock()
	if ctl != nil {
		return ctl.Snapshot()
	}
	return readPolicyFile(path)
}

func (g *Gateway) modelNames() []string {
	g.mu.RLock()
	r := g.router
	g.mu.RUnlock()
	if r == nil {
		return []string{}
	}
	return r.Names()
}

func (g *Gateway) modelDefault() string {
	g.mu.RLock()
	r := g.router
	g.mu.RUnlock()
	if r == nil {
		return ""
	}
	return r.DefaultName()
}

// subAgentsEnabledOnDisk reads the sub-agents toggle from the config file so
// the PATCH response reflects what was just written (g.cfg is the boot
// snapshot and doesn't change until restart).
func (g *Gateway) subAgentsEnabledOnDisk() bool {
	g.mu.RLock()
	path := g.configPath
	g.mu.RUnlock()
	if path == "" {
		return g.cfg.SubAgents != nil && g.cfg.SubAgents.Enabled
	}
	m := readConfigMap(path)
	if sa, ok := m["subAgents"].(map[string]interface{}); ok {
		if en, ok := sa["enabled"].(bool); ok {
			return en
		}
	}
	return g.cfg.SubAgents != nil && g.cfg.SubAgents.Enabled
}

// swarmEnabledOnDisk reads the swarm toggle from the config file so the PATCH
// response reflects what was just written (g.cfg is the boot snapshot).
func (g *Gateway) swarmEnabledOnDisk() bool {
	g.mu.RLock()
	path := g.configPath
	g.mu.RUnlock()
	if path == "" {
		return g.cfg.Swarm != nil && g.cfg.Swarm.Enabled
	}
	m := readConfigMap(path)
	if sw, ok := m["swarm"].(map[string]interface{}); ok {
		if en, ok := sw["enabled"].(bool); ok {
			return en
		}
	}
	return g.cfg.Swarm != nil && g.cfg.Swarm.Enabled
}

// claudeCodeEnabledOnDisk reads the claude-code toggle from the config file so
// the PATCH response reflects what was just written (g.cfg is the boot snapshot).
func (g *Gateway) claudeCodeEnabledOnDisk() bool {
	g.mu.RLock()
	path := g.configPath
	g.mu.RUnlock()
	if path == "" {
		return g.cfg.ClaudeCode != nil && g.cfg.ClaudeCode.Enabled
	}
	m := readConfigMap(path)
	if cc, ok := m["claudeCode"].(map[string]interface{}); ok {
		if en, ok := cc["enabled"].(bool); ok {
			return en
		}
	}
	return g.cfg.ClaudeCode != nil && g.cfg.ClaudeCode.Enabled
}

// takeoverView builds the takeover section from the boot config (for GET).
func (g *Gateway) takeoverView() takeoverView {
	enabled, provider := false, "claude"
	if g.cfg.Takeover != nil {
		enabled = g.cfg.Takeover.Enabled
		if g.cfg.Takeover.Provider != "" {
			provider = g.cfg.Takeover.Provider
		}
	}
	return takeoverView{Enabled: enabled, Provider: provider, Providers: takeoverViewProviders}
}

// takeoverViewFromDisk reads the takeover block from the config file so the
// PATCH response reflects what was just written (g.cfg is the boot snapshot).
func (g *Gateway) takeoverViewFromDisk() takeoverView {
	g.mu.RLock()
	path := g.configPath
	g.mu.RUnlock()
	v := g.takeoverView() // start from boot config defaults
	if path == "" {
		return v
	}
	if tk, ok := readConfigMap(path)["takeover"].(map[string]interface{}); ok {
		if en, ok := tk["enabled"].(bool); ok {
			v.Enabled = en
		}
		if p, ok := tk["provider"].(string); ok && p != "" {
			v.Provider = p
		}
	}
	return v
}

// savePolicy writes the full policy struct back to disk. PolicyConfig is a
// closed shape (version/defaults/rules), so a typed round-trip preserves
// everything the engine knows about.
func (g *Gateway) savePolicy(pol types.PolicyConfig) error {
	g.mu.RLock()
	path := g.policyPath
	g.mu.RUnlock()
	if path == "" {
		return errNoSettingsPath
	}
	if pol.Version == 0 {
		pol.Version = 1
	}
	buf, err := yaml.Marshal(pol)
	if err != nil {
		return err
	}
	return atomicWrite(path, buf, 0o644)
}

// saveConfigKey does a surgical edit of config.yaml: it loads the file into
// a generic map, sets one nested key, and writes it back — preserving every
// other key the user has (including ones this struct doesn't model). Comments
// are lost (a known YAML round-trip limitation) but no data is.
func (g *Gateway) saveConfigKey(keyPath []string, value interface{}) error {
	g.mu.RLock()
	path := g.configPath
	g.mu.RUnlock()
	if path == "" {
		return errNoSettingsPath
	}
	m := readConfigMap(path)
	if m == nil {
		m = map[string]interface{}{}
	}
	cur := m
	for i, k := range keyPath {
		if i == len(keyPath)-1 {
			cur[k] = value
			break
		}
		next, ok := cur[k].(map[string]interface{})
		if !ok {
			next = map[string]interface{}{}
			cur[k] = next
		}
		cur = next
	}
	buf, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	return atomicWrite(path, buf, 0o644)
}

func (g *Gateway) auditSettingsChange(userID string, body []byte) {
	g.mu.RLock()
	rec := g.audit
	g.mu.RUnlock()
	if rec == nil {
		return
	}
	_ = rec.Log("", userID, "settings_change", map[string]interface{}{
		"patch": string(body),
	})
}

// --- policy rule helpers ---

func hasRule(pol types.PolicyConfig, name string) bool {
	for _, r := range pol.Rules {
		if r.Name == name {
			return true
		}
	}
	return false
}

func removeRule(pol types.PolicyConfig, name string) types.PolicyConfig {
	out := pol.Rules[:0:0]
	for _, r := range pol.Rules {
		if r.Name != name {
			out = append(out, r)
		}
	}
	pol.Rules = out
	return pol
}

func ensureWebSearchRule(pol types.PolicyConfig) types.PolicyConfig {
	if hasRule(pol, webSearchRuleName) {
		return pol
	}
	domains := make([]interface{}, len(webSearchDomains))
	for i, d := range webSearchDomains {
		domains[i] = d
	}
	auditFalse := false
	pol.Rules = append(pol.Rules, types.PolicyRule{
		Name:  webSearchRuleName,
		When:  map[string]interface{}{"action": "network"},
		Allow: map[string]interface{}{"network": domains},
		Audit: &auditFalse,
	})
	return pol
}

func boolToMode(b bool, whenTrue, whenFalse string) string {
	if b {
		return whenTrue
	}
	return whenFalse
}

// --- file helpers (no internal/config import → no package cycle) ---

func readConfigMap(path string) map[string]interface{} {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]interface{}{}
	}
	var m map[string]interface{}
	if err := yaml.Unmarshal(data, &m); err != nil || m == nil {
		return map[string]interface{}{}
	}
	return m
}

func readPolicyFile(path string) types.PolicyConfig {
	def := types.PolicyConfig{
		Version: 1,
		Defaults: types.PermissionSet{
			Network: "deny", Filesystem: "read-only", Shell: "deny", Secrets: "isolated",
		},
	}
	if path == "" {
		return def
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return def
	}
	var pol types.PolicyConfig
	if err := yaml.Unmarshal(data, &pol); err != nil {
		return def
	}
	if pol.Version == 0 {
		pol.Version = 1
	}
	if pol.Defaults.Network == "" {
		pol.Defaults.Network = def.Defaults.Network
	}
	if pol.Defaults.Filesystem == "" {
		pol.Defaults.Filesystem = def.Defaults.Filesystem
	}
	if pol.Defaults.Shell == "" {
		pol.Defaults.Shell = def.Defaults.Shell
	}
	if pol.Defaults.Secrets == "" {
		pol.Defaults.Secrets = def.Defaults.Secrets
	}
	return pol
}

// atomicWrite writes via a tempfile + rename so a crash mid-write can't
// leave a half-written (and possibly permissive) policy/config file.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
