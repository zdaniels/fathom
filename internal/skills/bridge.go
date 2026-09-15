package skills

import (
	"context"
	"errors"
	"github.com/zdaniels/fathom/internal/brandenv"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/zdaniels/fathom/internal/agent"
	"github.com/zdaniels/fathom/internal/security"
	"github.com/zdaniels/fathom/pkg/types"
)

// BridgeOptions configures Bridge.
type BridgeOptions struct {
	SkillsDir string
	Vault     *security.SecretsVault
	Egress    *security.EgressProxy // in-process proxy (nil when external is used)
	Only      []string              // optional whitelist of skill folder names

	// ChasmRuntime, when set, overrides the default subprocess sandbox.
	// Each skill invocation goes to Chasm (POST /run → transient
	// container) instead of running as a child node process.
	ChasmRuntime *ChasmRuntime

	// ExternalEgressURL is the URL of an external egress proxy (Charon)
	// when in-process Egress is nil. The bridge stamps it onto each
	// tool's EgressContext so the runner can set HTTP_PROXY inside the
	// skill process/container.
	ExternalEgressURL   string
	ExternalEgressToken string
}

// BridgeResult is what Bridge returns: the registry it populated, the tools
// it produced, and any skills that failed to install.
type BridgeResult struct {
	Registry *Registry
	Tools    []agent.ToolDefinition
	Failed   []FailedInstall
}

// FailedInstall describes a single install failure for operator logs.
type FailedInstall struct {
	Name  string
	Error string
}

// Bridge walks SkillsDir, installs each skill into a fresh registry, and
// produces ToolDefinitions whose Execute method routes through Registry.Invoke
// (which spawns a sandboxed subprocess and injects per-skill secrets +
// egress). Tool names are namespaced as <skill>_<tool> to dodge collisions
// across skills (slack/discord/telegram all expose send_message).
func Bridge(opts BridgeOptions) BridgeResult {
	registry := NewRegistry(NewSandbox("", 60_000))
	res := BridgeResult{Registry: registry}

	// Ensure both runners are extracted; rebuild the sandbox wired up
	// for both Node and Python skills. A missing python3 binary on the
	// host machine is fine — the Python runner just sits on disk and
	// only matters when a manifest declares `language: python`.
	runnerPath, err := EnsureRunnerOnDisk()
	if err != nil {
		slog.Error("failed to extract runner.js", "err", err)
	} else {
		sb := NewSandbox(runnerPath, 60_000)
		if pyPath, perr := EnsurePythonRunnerOnDisk(); perr == nil {
			sb = sb.WithPythonRunner(pyPath)
		} else {
			slog.Warn("failed to extract runner.py; python skills will error at invoke time", "err", perr)
		}
		// Deny skill subprocesses OS-level access to every on-disk secret
		// (vault, master key, raw API token, device-store DB) so a malicious
		// skill can't read them straight off disk, bypassing the egress-proxy
		// "skills never see raw secrets" model. No-op where no sandbox tool is
		// available. See security.SensitiveLocalPaths for the exact set.
		sb = sb.WithIsolation(security.SensitiveLocalPaths()...)
		registry.sandbox = sb
		registry.runtime = registry.sandbox // refresh the runtime ptr too
	}

	// Swap in the Chasm runtime when configured. The subprocess Sandbox
	// stays constructed (it's the fallback if SetRuntime is later
	// reverted) but no calls flow through it once the Chasm runtime is
	// installed.
	if opts.ChasmRuntime != nil {
		registry.SetRuntime(opts.ChasmRuntime)
	}

	if _, err := os.Stat(opts.SkillsDir); err != nil {
		slog.Info("skills dir does not exist; nothing to bridge", "dir", opts.SkillsDir)
		return res
	}
	entries, err := os.ReadDir(opts.SkillsDir)
	if err != nil {
		slog.Warn("readdir skills failed", "dir", opts.SkillsDir, "err", err)
		return res
	}

	onlySet := map[string]bool{}
	for _, n := range opts.Only {
		onlySet[n] = true
	}
	// Distinguish "no filter" (nil) from "explicit empty whitelist"
	// (non-nil zero-length slice). The routing layer uses the latter when
	// a profile resolves to no skills — without this distinction, Bridge
	// would treat that as "no filter" and bridge everything, leaking
	// the wrong tools into the wrong profile.
	hasFilter := opts.Only != nil

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if hasFilter && !onlySet[name] {
			continue
		}
		skillDir := filepath.Join(opts.SkillsDir, name)
		if _, err := os.Stat(filepath.Join(skillDir, "skill.manifest.yaml")); err != nil {
			continue
		}
		installed, err := registry.Install(skillDir)
		if err != nil {
			slog.Warn("skill install failed", "skill", name, "err", err)
			res.Failed = append(res.Failed, FailedInstall{Name: name, Error: err.Error()})
			continue
		}
		if len(installed.Manifest.Tools) == 0 {
			slog.Info("skill has no tools declared in manifest; skipping bridge", "skill", installed.Manifest.Name)
			continue
		}
		allowedHosts := ExtractAllowedHosts(installed.Manifest)
		declaredSecrets := installed.Manifest.Permissions.Secrets
		for _, meta := range installed.Manifest.Tools {
			res.Tools = append(res.Tools, makeBridgedTool(
				registry, opts.Vault, opts.Egress,
				opts.ExternalEgressURL, opts.ExternalEgressToken,
				installed.Manifest.Name, meta, allowedHosts, declaredSecrets,
			))
		}
		slog.Info("skill bridged",
			"skill", installed.Manifest.Name,
			"tools", len(installed.Manifest.Tools),
			"hosts", len(allowedHosts),
		)
	}
	return res
}

// makeBridgedTool wraps a single SkillToolMeta in a ToolDefinition whose
// Execute pre-resolves the skill's declared secrets via the vault (scoped
// to this skill name), issues a per-invocation egress token, and calls
// Registry.Invoke.
func makeBridgedTool(
	registry *Registry,
	vault *security.SecretsVault,
	egress *security.EgressProxy,
	externalEgressURL, externalEgressToken string,
	skillName string,
	meta types.SkillToolMeta,
	allowedHosts []string,
	declaredSecrets []string,
) agent.ToolDefinition {
	namespaced := strings.ReplaceAll(skillName, "-", "_") + "_" + meta.Name
	return agent.ToolDefinition{
		Name:        namespaced,
		Description: "[" + skillName + "] " + meta.Description,
		Parameters:  meta.Parameters,
		SkillName:   skillName,
		Execute: func(ctx context.Context, p map[string]interface{}, _ agent.ToolContext) (interface{}, error) {
			secrets := map[string]string{}
			if vault != nil {
				secrets = vault.InjectForRequest(declaredSecrets, skillName)
			}
			var egressCtx *EgressContext
			// External egress (Charon) wins when configured — skill sees
			// HTTP_PROXY pointing at it. Fathom doesn't issue per-call
			// tokens in that mode; Charon owns the credential boundary.
			if externalEgressURL != "" {
				egressCtx = &EgressContext{URL: externalEgressURL, Token: externalEgressToken}
			} else if egress != nil && len(allowedHosts) > 0 {
				tok, err := egress.IssueToken(skillName, allowedHosts, 5*60*1e9)
				if err != nil {
					return nil, err
				}
				defer egress.RevokeToken(tok)
				egressCtx = &EgressContext{URL: egress.URL(), Token: tok}
			}
			return registry.Invoke(ctx, skillName, Invocation{
				FunctionName: meta.Function,
				Input:        p,
				Secrets:      secrets,
				Egress:       egressCtx,
			})
		},
	}
}

// ExtractAllowedHosts pulls bare hostname patterns from manifest.permissions.network.
// Each network entry is "METHOD https://host/path/*" — we want just `host`.
// Plain hostnames or "host/path/*" are accepted for forward compat.
func ExtractAllowedHosts(m types.SkillManifest) []string {
	rules := m.Permissions.Network
	seen := map[string]struct{}{}
	out := []string{}
	for _, raw := range rules {
		cleaned := stripMethodPrefix(raw)
		if cleaned == "" {
			continue
		}
		host := ""
		if strings.HasPrefix(strings.ToLower(cleaned), "http://") || strings.HasPrefix(strings.ToLower(cleaned), "https://") {
			u, err := url.Parse(strings.ReplaceAll(cleaned, "*", "X"))
			if err == nil {
				host = u.Hostname()
			}
		} else {
			host = strings.SplitN(cleaned, "/", 2)[0]
		}
		if host == "" {
			continue
		}
		if _, dup := seen[host]; dup {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
	}
	return out
}

var methodPrefix = []string{"GET ", "POST ", "PUT ", "DELETE ", "PATCH ", "HEAD ", "* "}

func stripMethodPrefix(s string) string {
	t := strings.TrimSpace(s)
	upper := strings.ToUpper(t)
	for _, m := range methodPrefix {
		if strings.HasPrefix(upper, m) {
			return strings.TrimSpace(t[len(m):])
		}
	}
	return t
}

// DefaultSkillsDir resolves the skills directory in this priority order:
//
//  1. $FANTAZM_SKILLS_DIR if set                      (explicit override)
//  2. <cwd>/skills/      if it contains any skill    (per-project install)
//  3. ~/.local/share/fantazm/skills/                  (global / --global install)
//
// The cwd check requires actual skills present (not just an empty dir) so
// the launchd-spawned gateway — which starts in $HOME — falls through to
// the global location instead of looking at $HOME/skills/. Per-project
// skill dirs still win when fathom chat / fathom start runs from a project
// root that has them.
func DefaultSkillsDir() string {
	if v := brandenv.Get("FATHOM_SKILLS_DIR"); v != "" {
		return v
	}
	if cwd, err := os.Getwd(); err == nil {
		local := filepath.Join(cwd, "skills")
		if HasInstallableSkills(local) {
			return local
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "fantazm", "skills")
	}
	// Last-resort fallback: cwd/skills even if empty (preserves the prior
	// behaviour for callers in tests with no $HOME).
	cwd, _ := os.Getwd()
	return filepath.Join(cwd, "skills")
}

// HasInstallableSkills reports whether dir contains any subdir with a
// skill.manifest.yaml. Used by the agent factory to decide whether to start
// the egress proxy.
func HasInstallableSkills(dir string) bool {
	if _, err := os.Stat(dir); err != nil {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "skill.manifest.yaml")); err == nil {
			return true
		}
	}
	return false
}

// SkillsByCategory returns the names of skills in dir whose manifest's
// `category` field matches one of the given categories (lowercase-insensitive).
// Used by the routing layer to expand a profile's category whitelist into
// concrete skill names for skills.Bridge to honour. Skills without a category
// in their manifest are never matched.
func SkillsByCategory(dir string, categories map[string]bool) []string {
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		manifestPath := filepath.Join(dir, e.Name(), "skill.manifest.yaml")
		raw, err := os.ReadFile(manifestPath)
		if err != nil {
			continue
		}
		m, err := ParseManifest(raw)
		if err != nil {
			continue
		}
		if m.Category == "" {
			continue
		}
		if categories[strings.ToLower(m.Category)] {
			out = append(out, e.Name())
		}
	}
	return out
}

// _ silences unused import errors during partial builds.
var _ = errors.New
