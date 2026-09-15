// Package agentfactory composes every piece — vault, security mesh, LLM
// provider, tool registry, persona, builtin tools, sandboxed skills, egress
// proxy — into one ready-to-handle-messages agent.
//
// Sitting in its own package keeps the dependency graph tidy: agent +
// security + skills + builtin all flow into here, the gateway flows out.
package agentfactory

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/zdaniels/fathom/internal/agent"
	"github.com/zdaniels/fathom/internal/agent/llm"
	"github.com/zdaniels/fathom/internal/builtin"
	"github.com/zdaniels/fathom/internal/config"
	"github.com/zdaniels/fathom/internal/integrations/beaconclient"
	"github.com/zdaniels/fathom/internal/integrations/charonclient"
	"github.com/zdaniels/fathom/internal/memory"
	"github.com/zdaniels/fathom/internal/security"
	"github.com/zdaniels/fathom/internal/skills"
	"github.com/zdaniels/fathom/pkg/types"
)

// Options tunes what the factory builds.
type Options struct {
	// EnabledSkills limits the starter skills installed. Empty = all
	// (notes, web-search, file-editor).
	EnabledSkills []string

	// ExtraTools are appended after starters and bridged installed skills.
	// Used by tests / advanced callers.
	ExtraTools []agent.ToolDefinition

	// PersonaOverride forces the persona text; empty falls back to
	// LoadPersona() reading from disk.
	PersonaOverride string

	// VaultOverride supplies an already-open vault (tests, in-memory).
	// Empty means OpenDefaultVault from disk.
	VaultOverride *security.SecretsVault

	// Egress starts the proxy even when no skills bridge would need it.
	// Auto-enabled when InstalledSkills is true AND skills exist on disk.
	Egress bool

	// InstalledSkills bridges /skills/* into the tool registry. Default true.
	// Set false to skip the skill walk entirely (useful for tests / minimal
	// startups).
	InstalledSkills *bool

	// SkillsDir overrides where to look for skill folders.
	SkillsDir string
}

// Usage is the per-message token count snapshot. Same shape as
// llm.Usage; redeclared here so callers don't have to import the
// LLM package just to type their cost-tracking code.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Result is what the factory returns to the gateway/CLI.
type Result struct {
	Ready       bool
	Description string
	Handler     func(ctx context.Context, msg types.ChannelMessage, sess types.Session) (string, error)
	HandlerN    func(ctx context.Context, msg types.ChannelMessage, sess types.Session, model string) (string, error)
	// HandlerNU returns aggregated token Usage + the resolved model
	// name alongside the reply. Used by the threads API to persist
	// per-message cost info. nil for legacy / minimal setups.
	HandlerNU   func(ctx context.Context, msg types.ChannelMessage, sess types.Session, model string) (string, *Usage, string, error)
	Router      *llm.Router
	Loop        *agent.Loop
	Vault       *security.SecretsVault
	Security    *security.Mesh
	EgressProxy *security.EgressProxy
	Memory      *memory.Bridge       // nil if recall sidecar isn't installed
	Beacon      *beaconclient.Client // nil when telemetry not configured; callers should Stop() on shutdown
}

// CreateDefault assembles everything. Returns Ready=false (with an echo
// handler) when no LLM API key is configured — lets the gateway start
// even before the user has run `fathom init`.
//
// When cfg.Routing is set with non-empty Profiles, dispatches to
// CreateRouted (intent-classifier + per-profile loops with filtered tool
// sets). Otherwise builds the legacy single-agent path.
func CreateDefault(cfg types.Config, opts Options) (*Result, error) {
	if takeoverEnabled(cfg) {
		return createTakeover(cfg, opts)
	}
	if cfg.Routing != nil && len(cfg.Routing.Profiles) > 0 {
		if r, err := CreateRouted(cfg, opts); err != nil {
			return nil, err
		} else if r != nil {
			return r, nil
		}
	}
	loadDotEnv()

	vault, mesh, err := openSecurity(cfg, opts)
	if err != nil {
		return nil, err
	}
	getSecret := secretGetter(vault)
	router, err := llm.NewRouter(cfg.LLM, func(name string) (string, error) { return getSecret(name, "") })
	if err != nil {
		var missing *llm.MissingCredentialError
		if !errors.As(err, &missing) {
			return nil, err
		}
		return &Result{
			Ready: false, Description: "echo (" + err.Error() + "; run 'fathom init')",
			Vault: vault, Security: mesh,
			Handler: func(ctx context.Context, msg types.ChannelMessage, _ types.Session) (string, error) {
				return "[fathom] Echo: " + msg.Text + "\n\nAgent setup incomplete: " + missing.Error(), nil
			},
		}, nil
	}

	// Tool registry.
	tools := agent.NewToolRegistry(mesh.Policy, mesh.Audit, mesh.Canary)
	starter := builtin.All()
	if len(opts.EnabledSkills) > 0 {
		starter = builtin.ForSkills(opts.EnabledSkills)
	}
	starter = append(starter, claudeCodeTools(cfg)...) // gated; nil when disabled
	for _, t := range starter {
		tools.Register(t)
	}
	for _, t := range opts.ExtraTools {
		tools.Register(t)
	}
	tools.SetSecretResolver(getSecret)

	// Minimal profile strips skill bridging, egress, memory, and external
	// runtimes (Chasm/Beacon) — the chat-brain posture. Keeps agent loop,
	// vault, security mesh, builtin tools.
	minimal := cfg.Profile == types.ProfileMinimal

	// Bridge installed skills if any are on disk and the option allows.
	wantInstalled := !minimal
	if opts.InstalledSkills != nil {
		wantInstalled = *opts.InstalledSkills
	}
	skillsDir := opts.SkillsDir
	if skillsDir == "" {
		skillsDir = skills.DefaultSkillsDir()
	}
	skillsExist := wantInstalled && skills.HasInstallableSkills(skillsDir)

	// External egress (Charon) takes precedence: when cfg.Egress.Proxy
	// is set, Fathom skips its in-process EgressProxy and routes skill
	// outbound traffic through the external service via HTTP_PROXY env.
	// When unset, the legacy in-process proxy is started exactly as
	// before — fully backwards compatible. Minimal profile drops all
	// outbound routing concerns since there are no skills to route for.
	var externalEgress *charonclient.Config
	if !minimal {
		externalEgress, err = resolveExternalEgress(cfg.Egress)
		if err != nil {
			return nil, err
		}
	}

	var egress *security.EgressProxy
	if externalEgress == nil && (opts.Egress || skillsExist) {
		egress = security.NewEgressProxy(security.EgressProxyOptions{Vault: vault})
		if _, err := egress.Start(); err != nil {
			return nil, fmt.Errorf("egress proxy start: %w", err)
		}
	}

	// External skill runtime (Chasm): replaces the in-process subprocess
	// sandbox with HTTP calls to a Chasm server. Each invocation runs in
	// a transient hardened container instead of a Node subprocess. Only
	// activated when cfg.Skills.Runtime == "chasm". Skipped under
	// minimal profile (no skills means no need for a sandbox).
	var chasmRuntime *skills.ChasmRuntime
	if !minimal {
		chasmRuntime, err = resolveChasmRuntime(cfg.Skills)
		if err != nil {
			return nil, err
		}
	}

	// Telemetry (Beacon): ships per-iteration + per-tool spans. Nil when
	// cfg.Telemetry is absent — Loop falls back to its no-op tracer.
	// Honored even under minimal — observability is cheap and arguably
	// more important the leaner you go.
	beacon, err := resolveBeaconClient(cfg.Telemetry)
	if err != nil {
		return nil, err
	}

	var installedSkillCount, installedToolCount int
	hasEgressForBridge := egress != nil || externalEgress != nil
	if skillsExist && hasEgressForBridge {
		bridged := skills.Bridge(skills.BridgeOptions{
			SkillsDir:           skillsDir,
			Vault:               vault,
			Egress:              egress,       // nil when external is used
			ChasmRuntime:        chasmRuntime, // nil when subprocess runtime is used
			ExternalEgressURL:   urlOrEmpty(externalEgress),
			ExternalEgressToken: tokenOrEmpty(externalEgress),
		})
		for _, t := range bridged.Tools {
			tools.Register(t)
		}
		installedToolCount = len(bridged.Tools)
		seen := map[string]struct{}{}
		for _, t := range bridged.Tools {
			seen[t.SkillName] = struct{}{}
		}
		installedSkillCount = len(seen)
	}

	persona := opts.PersonaOverride
	if persona == "" {
		if p := agent.LoadPersona(); p != nil {
			persona = p.Text
		}
	}

	// Spawn the Fathom Recall memory sidecar (recall) if it's installed. Failure is
	// non-fatal — Fathom runs without memory and the user can install recall
	// later. See github.com/zdaniels/recall for the binary. Skipped
	// under minimal profile to keep the chat-brain posture pure.
	var memBridge *memory.Bridge
	memCount := 0
	if !minimal {
		if recallBin, _ := exec.LookPath("recall"); recallBin != "" {
			if b, err := memory.Spawn(context.Background(), recallBin); err == nil {
				memBridge = b
				memTools := memory.AsTools(context.Background(), b)
				for _, t := range memTools {
					tools.Register(t)
				}
				memCount = len(memTools)
			}
		}
	}

	// Multi-agent tools (delegate / swarm, gated). Non-routed agent has no
	// classifier, so "auto" model selection defers to the router default.
	if coordinationEnabled(cfg) {
		for _, t := range buildDelegateTools(cfg, mesh, router, getSecret, memBridge, nil) {
			tools.Register(t)
		}
	}

	loop := agent.NewLoop(agent.LoopOptions{
		Router: router, Tools: tools, Mesh: mesh, Persona: persona,
		Tracer: tracerOrNil(beacon),
	})

	// Register the critique tool once the loop + router are ready — it needs
	// both. Skipped when there's only one model registered (no critic to
	// dispatch to).
	if len(router.Names()) > 1 {
		tools.Register(builtin.CritiqueTool(loop, router))
	}

	// Description: prefer the router's view (sees multi-model setups). Fall
	// back to legacy single-model fields.
	var desc string
	names := router.Names()
	_, defName := router.Default()
	if len(names) > 1 {
		desc = "router(" + strings.Join(names, ",") + "; default=" + defName + ")"
	} else {
		desc = cfg.LLM.Provider + ":" + cfg.LLM.Model
		if m, ok := cfg.LLM.Models[defName]; ok {
			desc = m.Provider + ":" + m.Model
		}
	}
	desc += " (" + asInt(len(tools.Names())) + " tools"
	if installedSkillCount > 0 {
		desc += " + " + asInt(installedSkillCount) + " skill"
		if installedSkillCount > 1 {
			desc += "s"
		}
		desc += " (" + asInt(installedToolCount) + " tools)"
	}
	if memCount > 0 {
		desc += " + memory"
	}
	if persona != "" {
		desc += " + persona"
	}
	if egress != nil {
		u, _ := url.Parse(egress.URL())
		if u != nil {
			desc += " + egress@" + u.Port()
		}
	}
	if beacon != nil {
		desc += " + beacon"
	}
	desc += ")"

	handler := func(ctx context.Context, msg types.ChannelMessage, sess types.Session) (string, error) {
		return loop.Process(ctx, msg, sess)
	}
	handlerN := func(ctx context.Context, msg types.ChannelMessage, sess types.Session, model string) (string, error) {
		return loop.ProcessWithModel(ctx, msg, sess, model)
	}

	_ = memBridge // captured below in Result so chat/start can Stop() it
	return &Result{
		Ready:       true,
		Memory:      memBridge,
		Description: desc,
		Handler:     handler,
		HandlerN:    handlerN,
		Router:      router,
		Loop:        loop,
		Vault:       vault,
		Security:    mesh,
		EgressProxy: egress,
		Beacon:      beacon,
	}, nil
}

// secretGetter returns the (name, requestingSkill) → value resolver: vault
// first (scope-checked), then process.env as a fallback for keys not yet in
// the vault. Returns an error when neither source has it.
func secretGetter(vault *security.SecretsVault) func(name, skill string) (string, error) {
	return func(name, skill string) (string, error) {
		if vault != nil && vault.IsUnlocked() && vault.Has(name) {
			return vault.Get(name, skill)
		}
		if v := os.Getenv(name); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("required secret missing: %s (not in vault or .env)", name)
	}
}

// loadDotEnv mirrors the TS loader — reads .env in cwd into process.env
// without overwriting existing values. Centralised here so every entrypoint
// (CLI chat, gateway start, scheduler) picks up keys without extra steps.
func loadDotEnv() {
	cwd, _ := os.Getwd()
	path := filepath.Join(cwd, ".env")
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		idx := strings.IndexByte(line, '=')
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.Trim(strings.TrimSpace(line[idx+1:]), `"'`)
		if key == "" || os.Getenv(key) != "" {
			continue
		}
		_ = os.Setenv(key, val)
	}
}

func asInt(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// _ silences errors-import when partial builds elide other uses.
var _ = errors.New

func openSecurity(cfg types.Config, opts Options) (*security.SecretsVault, *security.Mesh, error) {
	vault := opts.VaultOverride
	if vault == nil {
		key, err := security.LoadOrCreateMasterKey()
		if err != nil {
			return nil, nil, fmt.Errorf("ensure keyfile: %w", err)
		}
		v, err := security.OpenVault(security.VaultOpenOptions{
			Path: security.DefaultVaultPath(),
			Key:  key,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("open vault: %w", err)
		}
		vault = v
	}

	// Build the security mesh first so it's available to the echo path too.
	policyPath := config.ResolvePolicyPath(cfg)
	policy := config.LoadPolicy(policyPath)
	mesh := security.NewMesh(policy, security.MeshOptions{Secrets: vault})
	if opts.VaultOverride == nil {
		audit, err := security.OpenAuditLogger(security.AuditPath(cfg.DataDir))
		if err != nil {
			return nil, nil, err
		}
		mesh.Audit = audit
	}

	return vault, mesh, nil
}
