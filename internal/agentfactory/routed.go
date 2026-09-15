// Routed agent factory.
//
// When cfg.Routing is set with non-empty Profiles, Fathom constructs one
// agent Loop per profile up-front: each loop gets its own filtered
// ToolRegistry (only the skills the profile names) and its own Provider
// (the profile's model). At request time, a single classifier LLM call
// picks the right profile and dispatches to its pre-built loop.
//
// This trades a tiny bit of memory (N loops instead of one) for a big
// win: every request's prompt context shrinks to just the relevant tools,
// which is the difference between "qwen3:14b is slow and flaky with 38
// tools" and "qwen3:14b is responsive and reliable with 5 tools".
//
// Shared (created once, used by all profile loops): vault, mesh, egress
// proxy, memory bridge, persona text.
// Per profile: provider, tool registry, system prompt addendum.

package agentfactory

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os/exec"
	"sort"
	"strings"

	"github.com/zdaniels/fathom/internal/agent"
	"github.com/zdaniels/fathom/internal/agent/llm"
	"github.com/zdaniels/fathom/internal/agent/router"
	"github.com/zdaniels/fathom/internal/builtin"
	"github.com/zdaniels/fathom/internal/config"
	"github.com/zdaniels/fathom/internal/integrations/beaconclient"
	"github.com/zdaniels/fathom/internal/memory"
	"github.com/zdaniels/fathom/internal/security"
	"github.com/zdaniels/fathom/internal/skills"
	"github.com/zdaniels/fathom/pkg/types"
)

// profileRuntime is one fully-assembled per-profile loop + provider.
type profileRuntime struct {
	name       string
	loop       *agent.Loop
	toolCount  int
	skillCount int
	modelDesc  string
}

// CreateRouted builds a routing-aware Result. Each profile gets its own
// Loop with a filtered tool set. Returns nil if cfg.Routing is unset or
// has no profiles — caller should then use CreateDefault.
//
// This wraps the same plumbing as CreateDefault (vault, mesh, egress,
// memory) so the per-profile loops share state where it makes sense and
// are isolated where the routing point requires it (provider + tools).
func CreateRouted(cfg types.Config, opts Options) (*Result, error) {
	if cfg.Routing == nil || len(cfg.Routing.Profiles) == 0 {
		return nil, nil
	}
	loadDotEnv()

	// Shared state: vault, mesh, egress, memory.
	shared, err := buildSharedRuntime(cfg, opts)
	if err != nil {
		return nil, err
	}

	// Build the classifier first — fail fast if it's misconfigured before
	// we spin up N loops.
	rtr, err := router.New(router.Options{
		Config:    *cfg.Routing,
		LLMConfig: cfg.LLM,
		Lookup: func(name string) (string, error) {
			return shared.getSecret(name, "")
		},
	})
	if err != nil {
		return nil, fmt.Errorf("router: %w", err)
	}
	if rtr == nil {
		return nil, fmt.Errorf("router: empty profile set (this is a bug — CreateRouted should not have been called)")
	}

	// Build the multi-agent tools (delegate / swarm) once and register them
	// into every profile registry below. Gated — off unless sub-agents or
	// swarm is enabled in config.
	if coordinationEnabled(cfg) {
		shared.delegateTools = buildDelegateTools(cfg, shared.mesh, shared.router, shared.getSecret, shared.memBridge, rtr)
	}

	// Per-profile loops, keyed by profile name.
	runtimes := make(map[string]*profileRuntime, len(cfg.Routing.Profiles))
	profileNames := make([]string, 0, len(cfg.Routing.Profiles))
	for name := range cfg.Routing.Profiles {
		profileNames = append(profileNames, name)
	}
	sort.Strings(profileNames)
	for _, name := range profileNames {
		profile := cfg.Routing.Profiles[name]
		rt, err := buildProfileRuntime(name, profile, cfg, opts, shared)
		if err != nil {
			return nil, fmt.Errorf("profile %s: %w", name, err)
		}
		runtimes[name] = rt
	}

	// Description: list profiles + the router's classifier model.
	desc := buildRoutedDescription(cfg, runtimes, profileNames, shared)

	handler := func(ctx context.Context, msg types.ChannelMessage, sess types.Session) (string, error) {
		picked := rtr.Classify(ctx, msg.Text)
		rt, ok := runtimes[picked]
		if !ok {
			// shouldn't happen — Classify always returns a known profile or
			// the fallback, but defensive.
			rt = runtimes[rtr.Fallback()]
		}
		slog.Info("router: dispatching", "profile", rt.name, "model", rt.modelDesc, "tools", rt.toolCount)
		// IMPORTANT: dispatch via ProcessWithModel(rt.modelDesc), not Process().
		// Each profile pre-loop holds a SHARED llm.Router (so multi-model
		// /use still works). Plain Process() resolves the router's default
		// model — which is set once at startup and not per-profile, so
		// every request would go to the same model regardless of which
		// profile won. ProcessWithModel forces the named model.
		return rt.loop.ProcessWithModel(ctx, msg, sess, rt.modelDesc)
	}

	// HandlerN with explicit model name bypasses the router — useful for
	// /use <model> in chat. Routes through the profile whose model matches,
	// falling back to fallback if there's no exact match.
	handlerN := func(ctx context.Context, msg types.ChannelMessage, sess types.Session, modelName string) (string, error) {
		rt := pickRuntimeByModel(runtimes, profileNames, modelName)
		if rt == nil {
			rt = runtimes[rtr.Fallback()]
		}
		return rt.loop.ProcessWithModel(ctx, msg, sess, modelName)
	}

	// HandlerNU is HandlerN with usage data — same dispatch, returns
	// the cumulative token count + resolved model name alongside the
	// reply. Used by the threads API to persist per-message cost info.
	handlerNU := func(ctx context.Context, msg types.ChannelMessage, sess types.Session, modelName string) (string, *Usage, string, error) {
		rt := pickRuntimeByModel(runtimes, profileNames, modelName)
		if rt == nil {
			rt = runtimes[rtr.Fallback()]
		}
		r, err := rt.loop.ProcessV2(ctx, msg, sess, modelName)
		if err != nil {
			return "", nil, "", err
		}
		// Don't bother returning a zero-valued usage — saves callers
		// from writing useless "0 tokens" metadata onto every message.
		var u *Usage
		if r.Usage.PromptTokens > 0 || r.Usage.CompletionTokens > 0 {
			u = &Usage{
				PromptTokens:     r.Usage.PromptTokens,
				CompletionTokens: r.Usage.CompletionTokens,
			}
		}
		return r.Reply, u, r.ModelName, nil
	}

	return &Result{
		Ready:       true,
		Memory:      shared.memBridge,
		Description: desc,
		Handler:     handler,
		HandlerN:    handlerN,
		HandlerNU:   handlerNU,
		Router:      shared.router,
		Loop:        runtimes[rtr.Fallback()].loop, // expose fallback loop for callers that need one
		Vault:       shared.vault,
		Security:    shared.mesh,
		EgressProxy: shared.egress,
		Beacon:      shared.beacon,
	}, nil
}

// sharedRuntime bundles the everything-cluster shared across profile loops.
type sharedRuntime struct {
	vault     *security.SecretsVault
	mesh      *security.Mesh
	router    *llm.Router
	egress    *security.EgressProxy
	memBridge *memory.Bridge
	memCount  int
	persona   string
	getSecret func(name, scope string) (string, error)

	// External integrations — non-nil when fathom.config.yaml configures
	// them. Charon (external egress) and Chasm (external skill runtime)
	// can be enabled together or independently.
	externalEgressURL   string
	externalEgressToken string
	chasmRuntime        *skills.ChasmRuntime

	// Beacon telemetry client. Non-nil when cfg.Telemetry.BeaconURL is set.
	// Shared across profile loops so every profile's spans land in one place.
	beacon *beaconclient.Client

	// delegateTools are the sub-agent tools (`delegate` + `delegate_parallel`),
	// built once and registered into every profile registry. nil when
	// sub-agents are disabled (cfg.SubAgents.Enabled == false).
	delegateTools []agent.ToolDefinition
}

// defaultHardModelPreference is the ordered preference for difficulty=hard
// auto-selection: strong cloud first (only used if the key is configured,
// i.e. the name is registered), then the best local model.
var defaultHardModelPreference = []string{"opus", "sonnet", "qwen-large"}

// subAgentsEnabled reports whether the `delegate` tool should be wired.
func subAgentsEnabled(cfg types.Config) bool {
	return cfg.SubAgents != nil && cfg.SubAgents.Enabled
}

// swarmEnabled reports whether the `swarm` tool should be wired.
func swarmEnabled(cfg types.Config) bool {
	return cfg.Swarm != nil && cfg.Swarm.Enabled
}

// coordinationEnabled is true when either multi-agent tool set is on, so the
// shared DelegateDeps (child loops, model picker) gets built.
func coordinationEnabled(cfg types.Config) bool {
	return subAgentsEnabled(cfg) || swarmEnabled(cfg)
}

// claudeCodeEnabled reports whether the `claude_code` tool should be wired.
func claudeCodeEnabled(cfg types.Config) bool {
	return cfg.ClaudeCode != nil && cfg.ClaudeCode.Enabled
}

// claudeCodeTools builds the claude_code tool from config (default model +
// max turns), or nil when disabled. Returned as a slice so inject sites spread.
func claudeCodeTools(cfg types.Config) []agent.ToolDefinition {
	if !claudeCodeEnabled(cfg) {
		return nil
	}
	model, turns := "", 0
	if cfg.ClaudeCode != nil {
		model, turns = cfg.ClaudeCode.DefaultModel, cfg.ClaudeCode.MaxTurns
	}
	return []agent.ToolDefinition{builtin.ClaudeCodeTool(model, turns)}
}

// buildDelegateTool constructs the `delegate` sub-agent tool from the
// pieces both the routed and non-routed factories already have. The
// classifier may be nil (non-routed agent) — then "auto" model selection
// defers to the router default. Child registries are builtin-only
// (+ memory): no sandboxed-skill bridging in sub-agents for now, which
// keeps the blast radius small.
func buildDelegateTools(
	cfg types.Config,
	mesh *security.Mesh,
	llmRouter *llm.Router,
	getSecret func(name, skill string) (string, error),
	memBridge *memory.Bridge,
	classifier *router.Router,
) []agent.ToolDefinition {
	maxDepth := 0
	if cfg.SubAgents != nil {
		maxDepth = cfg.SubAgents.MaxDepth
	}

	buildChildTools := func(skillNames []string, readOnly bool) *agent.ToolRegistry {
		reg := agent.NewToolRegistry(mesh.Policy, mesh.Audit, mesh.Canary)
		var defs []agent.ToolDefinition
		if readOnly {
			defs = builtin.ReadOnlyTools()
		} else {
			defs = builtin.ForSkills(skillNames)
		}
		for _, t := range defs {
			reg.Register(t)
		}
		if memBridge != nil {
			for _, t := range memory.AsTools(context.Background(), memBridge) {
				reg.Register(t)
			}
		}
		reg.SetSecretResolver(getSecret)
		return reg
	}

	var pickModel func(ctx context.Context, task string) string
	if classifier != nil {
		pickModel = func(ctx context.Context, task string) string {
			profile := classifier.Classify(ctx, task)
			if cfg.Routing != nil {
				if pc, ok := cfg.Routing.Profiles[profile]; ok {
					return pc.Model // "" → router default
				}
			}
			return ""
		}
	}

	deps := agent.DelegateDeps{
		Router:              llmRouter,
		Mesh:                mesh,
		MaxDepth:            maxDepth,
		PickModel:           pickModel,
		HardModelPreference: defaultHardModelPreference,
		BuildChildTools:     buildChildTools,
	}

	// Sub-agents (`delegate`/`delegate_parallel`) and swarms (`swarm`) share
	// the same child-loop machinery; wire whichever is enabled.
	var tools []agent.ToolDefinition
	if subAgentsEnabled(cfg) {
		tools = append(tools, agent.NewSubAgentTools(deps)...)
	}
	if swarmEnabled(cfg) {
		rounds, budget := 0, 0
		if cfg.Swarm != nil {
			rounds, budget = cfg.Swarm.MaxRounds, cfg.Swarm.TokenBudget
		}
		tools = append(tools, agent.NewSwarmTool(deps, rounds, budget))
	}
	return tools
}

// buildSharedRuntime constructs the things every profile loop reuses. This
// is the same plumbing as CreateDefault sans the tool registry + loop —
// those are per-profile.
func buildSharedRuntime(cfg types.Config, opts Options) (*sharedRuntime, error) {
	vault := opts.VaultOverride
	if vault == nil {
		key, err := security.LoadOrCreateMasterKey()
		if err != nil {
			return nil, fmt.Errorf("ensure keyfile: %w", err)
		}
		v, err := security.OpenVault(security.VaultOpenOptions{
			Path: security.DefaultVaultPath(),
			Key:  key,
		})
		if err != nil {
			return nil, fmt.Errorf("open vault: %w", err)
		}
		vault = v
	}

	policy := config.LoadPolicy(config.ResolvePolicyPath(cfg))
	mesh := security.NewMesh(policy, security.MeshOptions{Secrets: vault})
	getSecret := secretGetter(vault)

	// LLM router (multi-model registry) — every per-profile Loop reuses this.
	llmRouter, err := llm.NewRouter(cfg.LLM, func(name string) (string, error) {
		return getSecret(name, "")
	})
	if err != nil {
		return nil, err
	}

	// External egress (Charon) wins when configured — skip the in-process
	// proxy entirely. Same precedence as the non-routed path.
	externalEgress, err := resolveExternalEgress(cfg.Egress)
	if err != nil {
		return nil, err
	}

	// Egress proxy — only stood up if at least one profile will actually
	// bridge skills AND external Charon isn't configured.
	var egress *security.EgressProxy
	if externalEgress == nil && (profilesNeedSkills(cfg.Routing) || opts.Egress) {
		egress = security.NewEgressProxy(security.EgressProxyOptions{Vault: vault})
		if _, err := egress.Start(); err != nil {
			return nil, fmt.Errorf("egress proxy start: %w", err)
		}
	}

	// Chasm runtime — replaces subprocess sandbox per-skill-call when
	// configured. Shared across all profiles' Loops.
	chasmRuntime, err := resolveChasmRuntime(cfg.Skills)
	if err != nil {
		return nil, err
	}

	// Beacon client — shared across profile loops. nil when telemetry is
	// not configured; each Loop falls back to its no-op tracer.
	beacon, err := resolveBeaconClient(cfg.Telemetry)
	if err != nil {
		return nil, err
	}

	persona := opts.PersonaOverride
	if persona == "" {
		if p := agent.LoadPersona(); p != nil {
			persona = p.Text
		}
	}

	// Memory bridge — also shared (one recall subprocess for everyone).
	var memBridge *memory.Bridge
	memCount := 0
	if recallBin, _ := exec.LookPath("recall"); recallBin != "" {
		if b, err := memory.Spawn(context.Background(), recallBin); err == nil {
			memBridge = b
			memCount = len(memory.AsTools(context.Background(), b))
		}
	}

	return &sharedRuntime{
		vault:               vault,
		mesh:                mesh,
		router:              llmRouter,
		egress:              egress,
		externalEgressURL:   urlOrEmpty(externalEgress),
		externalEgressToken: tokenOrEmpty(externalEgress),
		chasmRuntime:        chasmRuntime,
		beacon:              beacon,
		memBridge:           memBridge,
		memCount:            memCount,
		persona:             persona,
		getSecret:           getSecret,
	}, nil
}

// buildProfileRuntime assembles one fully-isolated Loop for a profile.
// Each loop has its own ToolRegistry filtered to the profile's skills/
// categories + builtins. The provider comes from the profile's named
// model (or the global default when unspecified).
func buildProfileRuntime(
	name string,
	profile types.ProfileConfig,
	cfg types.Config,
	opts Options,
	shared *sharedRuntime,
) (*profileRuntime, error) {
	// Tool registry — fresh per profile so registrations don't leak.
	tools := agent.NewToolRegistry(shared.mesh.Policy, shared.mesh.Audit, shared.mesh.Canary)
	// Builtins per profile. In routed mode, builtins are opt-in — the
	// whole point of profiles is keeping tool counts small. A profile
	// that lists no builtins gets none (just memory tools, registered
	// later). opts.EnabledSkills (set by chat/start when the user passes
	// --skills on the CLI) overrides per-profile builtins entirely.
	var starter []agent.ToolDefinition
	switch {
	case len(opts.EnabledSkills) > 0:
		starter = builtin.ForSkills(opts.EnabledSkills)
	case len(profile.Builtins) > 0:
		starter = builtin.ForSkills(profile.Builtins)
	}
	// claude_code (gated): available across profiles when enabled, so the
	// agent can hand off real coding work regardless of the routed profile.
	starter = append(starter, claudeCodeTools(cfg)...)
	for _, t := range starter {
		tools.Register(t)
	}
	for _, t := range opts.ExtraTools {
		tools.Register(t)
	}
	tools.SetSecretResolver(shared.getSecret)

	// Resolve which skill folder names this profile wants. Combination of
	// explicit Skills + Categories (resolved via manifest scan).
	skillsDir := opts.SkillsDir
	if skillsDir == "" {
		skillsDir = skills.DefaultSkillsDir()
	}
	allowed := resolveProfileSkills(profile, skillsDir)
	bridgedSkills := 0
	hasAnyEgress := shared.egress != nil || shared.externalEgressURL != ""
	if hasAnyEgress && skills.HasInstallableSkills(skillsDir) {
		bridged := skills.Bridge(skills.BridgeOptions{
			SkillsDir:           skillsDir,
			Vault:               shared.vault,
			Egress:              shared.egress, // nil when external is used
			Only:                allowed,
			ChasmRuntime:        shared.chasmRuntime,
			ExternalEgressURL:   shared.externalEgressURL,
			ExternalEgressToken: shared.externalEgressToken,
		})
		for _, t := range bridged.Tools {
			tools.Register(t)
		}
		seen := map[string]struct{}{}
		for _, t := range bridged.Tools {
			seen[t.SkillName] = struct{}{}
		}
		bridgedSkills = len(seen)
	}

	// Memory tools — register on every profile registry (they're stateless
	// reads / appends).
	if shared.memBridge != nil {
		for _, t := range memory.AsTools(context.Background(), shared.memBridge) {
			tools.Register(t)
		}
	}

	// Sub-agent tools — same instances across profiles. Empty when
	// sub-agents are disabled.
	for _, t := range shared.delegateTools {
		tools.Register(t)
	}

	// Persona for this profile: baseline + global persona + per-profile addendum.
	persona := shared.persona
	if profile.SystemPrompt != "" {
		if persona != "" {
			persona += "\n\n"
		}
		persona += strings.TrimSpace(profile.SystemPrompt)
	}

	// Pick the profile's model. If unspecified, use the llm router's default.
	loopOpts := agent.LoopOptions{
		Router: shared.router, Tools: tools, Mesh: shared.mesh, Persona: persona,
		Tracer: tracerOrNil(shared.beacon),
	}
	loop := agent.NewLoop(loopOpts)

	// Model description — for the Result.Description line.
	modelDesc := profile.Model
	if modelDesc == "" {
		_, modelDesc = shared.router.Default()
	}

	return &profileRuntime{
		name:       name,
		loop:       loop,
		toolCount:  len(tools.Names()),
		skillCount: bridgedSkills,
		modelDesc:  modelDesc,
	}, nil
}

// resolveProfileSkills produces the union of explicit skills + skills whose
// manifest category matches one of profile.Categories. Returns nil (not
// empty) when the profile has neither, which tells skills.Bridge "no
// whitelist, take everything" — but in routed mode an empty whitelist is
// almost certainly a misconfig, so we log a warning.
func resolveProfileSkills(profile types.ProfileConfig, skillsDir string) []string {
	if len(profile.Skills) == 0 && len(profile.Categories) == 0 {
		slog.Warn("routing profile has no skills or categories — bridging nothing", "profile", profile)
		return []string{} // explicit empty whitelist
	}
	seen := map[string]bool{}
	for _, s := range profile.Skills {
		seen[s] = true
	}
	if len(profile.Categories) > 0 {
		catSet := map[string]bool{}
		for _, c := range profile.Categories {
			catSet[strings.ToLower(c)] = true
		}
		matches := skills.SkillsByCategory(skillsDir, catSet)
		for _, m := range matches {
			seen[m] = true
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// profilesNeedSkills returns true if any profile has skills or categories
// to bridge. Used to decide whether to spin up the egress proxy.
func profilesNeedSkills(rc *types.RoutingConfig) bool {
	if rc == nil {
		return false
	}
	for _, p := range rc.Profiles {
		if len(p.Skills) > 0 || len(p.Categories) > 0 {
			return true
		}
	}
	return false
}

// pickRuntimeByModel finds the profile whose Model matches modelName, or
// nil. Used by HandlerN when the chat session passes /use <model>.
func pickRuntimeByModel(
	runtimes map[string]*profileRuntime,
	order []string,
	modelName string,
) *profileRuntime {
	for _, name := range order {
		if runtimes[name].modelDesc == modelName {
			return runtimes[name]
		}
	}
	return nil
}

// buildRoutedDescription renders the human-readable summary that appears
// in `fathom doctor` and the gateway startup banner.
func buildRoutedDescription(
	cfg types.Config,
	runtimes map[string]*profileRuntime,
	order []string,
	shared *sharedRuntime,
) string {
	var b strings.Builder
	b.WriteString("routed[classifier=")
	b.WriteString(cfg.Routing.Router.Model)
	b.WriteString("](")
	for i, name := range order {
		if i > 0 {
			b.WriteString(", ")
		}
		rt := runtimes[name]
		b.WriteString(name)
		b.WriteString(":")
		b.WriteString(rt.modelDesc)
		b.WriteString("/")
		fmt.Fprintf(&b, "%dt", rt.toolCount)
	}
	b.WriteString(")")
	if shared.memCount > 0 {
		b.WriteString(" + memory")
	}
	if shared.persona != "" {
		b.WriteString(" + persona")
	}
	if shared.egress != nil {
		if u, _ := url.Parse(shared.egress.URL()); u != nil {
			b.WriteString(" + egress@" + u.Port())
		}
	}
	if shared.beacon != nil {
		b.WriteString(" + beacon")
	}
	return b.String()
}
