// Package types holds the public, cross-package types Fathom exposes.
// Keeping them under pkg/ (instead of internal/) means future SDKs and
// external integrations get a stable import path.
package types

import "time"

// Mode is Fathom's deployment tier. Personal runs locally with no admin API;
// team mounts multitenancy + RBAC; enterprise adds SSO + compliance exports.
type Mode string

const (
	ModePersonal   Mode = "personal"
	ModeTeam       Mode = "team"
	ModeEnterprise Mode = "enterprise"
)

// AuthMode controls how the gateway authenticates incoming requests.
type AuthMode string

const (
	AuthModeToken   AuthMode = "token"
	AuthModePasskey AuthMode = "passkey"
	AuthModeOIDC    AuthMode = "oidc"
	AuthModeMTLS    AuthMode = "mtls"
)

// AuthConfig configures the gateway's auth surface.
type AuthConfig struct {
	Mode           AuthMode      `yaml:"mode"`
	RequireMFA     bool          `yaml:"requireMFA"`
	SessionTimeout time.Duration `yaml:"sessionTimeout"`
	DeviceBinding  bool          `yaml:"deviceBinding"`
}

// LLMConfig configures the LLM(s) Fathom uses.
//
// Two ways to set it up:
//
//  1. Single-model (the legacy / simplest form): set Provider+Model at the
//     top level. The agent factory registers it as the "default" entry.
//
//  2. Multi-model: populate Models with named entries and optionally point
//     Default at one of them. The chat session can /use any named model;
//     /review uses the "critic" entry by convention.
//
//     llm:
//     default: local
//     models:
//     local:   { provider: ollama,    model: qwen3:14b }
//     coding:  { provider: anthropic, model: claude-sonnet-4-5 }
//     critic:  { provider: openai,    model: gpt-4o }
type LLMConfig struct {
	// Legacy single-model fields. Used when Models is empty.
	Provider    string  `yaml:"provider,omitempty"`
	Model       string  `yaml:"model,omitempty"`
	BaseURL     string  `yaml:"baseUrl,omitempty"`
	MaxTokens   int     `yaml:"maxTokens,omitempty"`
	Temperature float32 `yaml:"temperature,omitempty"`

	// Multi-model registry. When non-empty, Provider/Model are ignored;
	// the named entries are what's available to /use.
	Default string                    `yaml:"default,omitempty"`
	Models  map[string]LLMModelConfig `yaml:"models,omitempty"`
}

// LLMModelConfig is one entry in LLMConfig.Models.
type LLMModelConfig struct {
	Provider    string  `yaml:"provider"`
	Model       string  `yaml:"model"`
	BaseURL     string  `yaml:"baseUrl,omitempty"`
	MaxTokens   int     `yaml:"maxTokens,omitempty"`
	Temperature float32 `yaml:"temperature,omitempty"`
}

// RoutingConfig configures intent-based agent routing.
//
// Each incoming user message is classified by a small fast model (Router.Model)
// into one of the named Profiles. The picked profile decides which LLM handles
// the message AND which subset of skills it sees as tools — keeping prompt
// context small per request so smaller local models stay snappy and reliable.
//
// Example:
//
//	routing:
//	  router:
//	    model: classifier        # name from llm.models — tiny fast model
//	    fallback: general        # profile to use if classification fails
//	  profiles:
//	    general:
//	      model: local           # name from llm.models
//	      categories: [memory, productivity]
//	    communication:
//	      model: local
//	      skills: [gmail, slack, whatsapp, telegram]
//	      systemPrompt: |
//	        You are concise. Confirm before sending anything.
//	    code:
//	      model: coder
//	      categories: [development]
//	      skills: [file-editor, bash, grep]
//
// When Routing is nil or has no Profiles, Fathom uses the single-agent
// path (every request sees every tool) — backwards-compatible default.
type RoutingConfig struct {
	Router   RouterConfig             `yaml:"router"`
	Profiles map[string]ProfileConfig `yaml:"profiles"`
}

// RouterConfig describes the classifier that picks a profile per request.
type RouterConfig struct {
	// Model is the name of an entry in llm.models — typically a small fast
	// classifier (e.g. qwen2.5:1.5b). The classifier prompt is hardcoded in
	// internal/agent/router; only the model is configurable.
	Model string `yaml:"model"`
	// Fallback names the profile to use when classification fails (model
	// errors, low-confidence reply, or unknown category in the output). If
	// empty, falls back to the first profile in lexicographic order.
	Fallback string `yaml:"fallback,omitempty"`
}

// ProfileConfig is one named routing target.
//
// Skills + Categories control which bridged (installed) skills the profile
// sees. Builtins controls which starter tools (notes, web-search,
// file-editor, shell, python, image-generation, system) it sees. Memory
// tools (recall_*, pin_for_session, record_decision) are always included
// — they're stateless and small.
//
// All three filters default to *empty* when omitted: a profile that lists
// nothing gets no skills and no builtins (just memory). That's deliberate
// — routing's whole point is keeping tool counts small. Use `builtins:
// [notes, web-search]` to explicitly include them.
type ProfileConfig struct {
	// Model names an entry in llm.models that handles requests routed here.
	// If empty, falls back to llm.default.
	Model string `yaml:"model,omitempty"`
	// Skills is an exact-match whitelist of skill folder names.
	Skills []string `yaml:"skills,omitempty"`
	// Categories matches skills by manifest `category` field — broader than
	// Skills, less brittle when adding new skills in the same category.
	Categories []string `yaml:"categories,omitempty"`
	// Builtins names the starter tools to include for this profile.
	// Recognised values: notes, web-search, file-editor, shell, python,
	// image-generation, system. Memory tools (recall_*) are always on.
	// Empty/omitted = no builtins.
	Builtins []string `yaml:"builtins,omitempty"`
	// SystemPrompt is appended after the baseline + global persona, for
	// per-role guidance (tone, confirmation behavior, etc).
	SystemPrompt string `yaml:"systemPrompt,omitempty"`
}

// Config is the top-level configuration loaded from fathom.config.yaml.
type RetentionConfig struct {
	AuditDays         int `yaml:"auditDays" json:"auditDays"`
	DeletedThreadDays int `yaml:"deletedThreadDays" json:"deletedThreadDays"`
}

type TaskConnection struct {
	WorkspaceID string `yaml:"workspaceId" json:"workspaceId"`
	Provider    string `yaml:"provider" json:"provider"`
	TokenSecret string `yaml:"tokenSecret" json:"-"`
	Site        string `yaml:"site,omitempty" json:"site,omitempty"`
	Email       string `yaml:"email,omitempty" json:"-"`
	Scope       string `yaml:"scope" json:"scope"`
}
type CollaborationConfig struct {
	Image       string           `yaml:"image" json:"image"`
	Connections []TaskConnection `yaml:"connections,omitempty" json:"-"`
}
type Config struct {
	Collaboration *CollaborationConfig `yaml:"collaboration,omitempty" json:"collaboration,omitempty"`
	Retention     *RetentionConfig     `yaml:"retention,omitempty" json:"retention,omitempty"`
	ConfigPath    string               `json:"-" yaml:"-"` // Absolute source path, retained for settings and policy resolution.
	Mode          Mode                 `yaml:"mode"`
	Profile       Profile              `yaml:"profile,omitempty"` // "" = default, "minimal" = lean
	Host          string               `yaml:"host"`
	Port          int                  `yaml:"port"`
	Auth          AuthConfig           `yaml:"auth"`
	LLM           LLMConfig            `yaml:"llm"`
	Routing       *RoutingConfig       `yaml:"routing,omitempty"`
	Egress        *EgressConfig        `yaml:"egress,omitempty"`
	Skills        *SkillsConfig        `yaml:"skills,omitempty"`
	Telemetry     *TelemetryConfig     `yaml:"telemetry,omitempty"`
	DataDir       string               `yaml:"dataDir"`
	PolicyFile    string               `yaml:"policyFile"`
	LogLevel      string               `yaml:"logLevel"`
	Enterprise    *EnterpriseBlock     `yaml:"enterprise,omitempty"`
	SubAgents     *SubAgentsConfig     `yaml:"subAgents,omitempty"`
	Swarm         *SwarmConfig         `yaml:"swarm,omitempty"`
	ClaudeCode    *ClaudeCodeConfig    `yaml:"claudeCode,omitempty"`
	Takeover      *TakeoverConfig      `yaml:"takeover,omitempty"`
	Integrations  *IntegrationsConfig  `yaml:"integrations,omitempty"`
}

// SubAgentsConfig gates the `delegate` tool, which lets the main agent
// hand a scoped task to a child agent loop running on a chosen model
// (local or cloud). Off by default — opt in explicitly.
type SubAgentsConfig struct {
	// Enabled registers the `delegate` tool on the agent. Default false.
	Enabled bool `yaml:"enabled"`
	// MaxDepth caps delegation nesting (parent depth 0). At the cap, a
	// child's `delegate` tool is omitted, so recursion can't run away.
	// 0 → default of 2.
	MaxDepth int `yaml:"maxDepth,omitempty"`
}

// SwarmConfig gates the `swarm` tool — a coordinator that runs several
// role-agents over multiple rounds against a shared blackboard. Builds on
// the sub-agent machinery; off by default. MaxDepth from SubAgentsConfig
// still bounds nesting (a swarm member can't launch another swarm).
type SwarmConfig struct {
	// Enabled registers the `swarm` tool on the top-level agent.
	Enabled bool `yaml:"enabled"`
	// MaxRounds caps coordination rounds per swarm. 0 → default (3); the
	// engine also enforces a hard ceiling regardless of config.
	MaxRounds int `yaml:"maxRounds,omitempty"`
	// TokenBudget caps total tokens across all members in a swarm run.
	// 0 → no token cap (rounds + wall-clock still bound it).
	TokenBudget int `yaml:"tokenBudget,omitempty"`
}

// ClaudeCodeConfig gates the `claude_code` tool, which hands coding tasks to
// the locally-installed Claude Code CLI under the user's subscription login.
// Off by default — it can read/write files in the workspace.
type ClaudeCodeConfig struct {
	// Enabled registers the `claude_code` tool on the agent.
	Enabled bool `yaml:"enabled"`
	// DefaultModel is the Claude model used when a call doesn't specify one
	// (e.g. "haiku", "sonnet", "opus", or a full id). Empty → Claude Code's
	// own default for the logged-in account.
	DefaultModel string `yaml:"defaultModel,omitempty"`
	// MaxTurns caps Claude Code's agentic loop per call. 0 → default (30).
	MaxTurns int `yaml:"maxTurns,omitempty"`
}

// TakeoverConfig routes EVERY user message straight to an external coding-agent
// CLI (the local routed agent is bypassed), with per-thread session continuity.
// Off by default. The provider runs on its subscription login, in the
// workspace. Same safety posture as the claude_code tool.
type TakeoverConfig struct {
	// Enabled turns takeover on.
	Enabled bool `yaml:"enabled"`
	// Provider selects the CLI: "claude" (default) or "codex".
	Provider string `yaml:"provider,omitempty"`
	// Model passed to the provider (provider-specific; e.g. "haiku", "opus",
	// or an OpenAI model id for codex). Empty → the provider's default.
	Model string `yaml:"model,omitempty"`
}

// IntegrationsConfig holds per-integration settings, notably the OAuth
// client IDs used by `fathom connect <service>` browser sign-in flows.
type IntegrationsConfig struct {
	GitHub *GitHubIntegration `yaml:"github,omitempty"`
}

// GitHubIntegration configures the GitHub connect (device-flow) sign-in.
// OAuthClientID is the client ID of a GitHub OAuth App with device flow
// enabled. Device-flow client IDs are not secret, so this is safe to
// commit in config. Register one at github.com/settings/developers
// (OAuth Apps → enable "Device Flow").
type GitHubIntegration struct {
	OAuthClientID string `yaml:"oauthClientId,omitempty"`
}

// Profile picks a feature set within a Mode. Today only "minimal"
// means anything — it strips scheduler / threads / audit / memory /
// skill bridging / egress from the boot path so the agent boots in
// ~150ms with a tiny RSS, matching the NanoClaw "just a chat brain"
// posture. Future profiles (e.g. "compliance-strict") can extend.
type Profile string

const (
	ProfileDefault Profile = "" // omit from YAML → full feature set
	ProfileMinimal Profile = "minimal"
)

// TelemetryConfig opts into shipping OpenTelemetry traces to an
// OTLP/HTTP receiver — typically Beacon
// (https://github.com/fantazmai/beacon), but any OTLP-compatible
// receiver works (OTEL Collector, Honeycomb, Datadog via the collector,
// etc.). When BeaconURL is empty, telemetry emission is a no-op.
//
//	telemetry:
//	  beacon_url:        http://127.0.0.1:4318
//	  beacon_token_file: ~/.beacon/token   # optional
//	  service_name:      fathom
type TelemetryConfig struct {
	BeaconURL       string `yaml:"beacon_url"`
	BeaconToken     string `yaml:"beacon_token,omitempty"`
	BeaconTokenFile string `yaml:"beacon_token_file,omitempty"`
	ServiceName     string `yaml:"service_name,omitempty"`
}

// EgressConfig optionally routes skill outbound HTTP through an external
// proxy instead of the in-process security.EgressProxy. When Proxy is
// unset, Fathom uses its built-in proxy as before (backwards-compatible).
//
// Most commonly this points at Charon (https://github.com/fantazmai/charon)
// so credentials are held outside the agent process. Any proxy that
// accepts the Charon-style wire protocol works.
//
//	egress:
//	  proxy: http://127.0.0.1:8889
//	  token_file: ~/.charon/token   # OR
//	  token: <literal-token>
type EgressConfig struct {
	Proxy     string `yaml:"proxy"`
	Token     string `yaml:"token,omitempty"`
	TokenFile string `yaml:"token_file,omitempty"`
}

// SkillsConfig selects how skills are executed. Default (Runtime unset
// or "subprocess") spawns the bundled node runner per call. Setting
// Runtime to "chasm" delegates to an external Chasm server — the same
// JSON-in/JSON-out contract, but each call runs in a hardened transient
// container instead of a Node subprocess.
//
//	skills:
//	  runtime: chasm
//	  chasm_url: http://127.0.0.1:8890
//	  chasm_token_file: ~/.chasm/token
//	  default_image: ghcr.io/fantazmai/chasm-node22:latest
type SkillsConfig struct {
	Runtime        string `yaml:"runtime,omitempty"`
	ChasmURL       string `yaml:"chasm_url,omitempty"`
	ChasmToken     string `yaml:"chasm_token,omitempty"`
	ChasmTokenFile string `yaml:"chasm_token_file,omitempty"`
	DefaultImage   string `yaml:"default_image,omitempty"`
}

// EnterpriseBlock holds enterprise-mode configuration; ignored unless
// Mode == enterprise.
type EnterpriseBlock struct {
	SSO   *SSOConfig   `yaml:"sso,omitempty"`
	SIEM  *SIEMConfig  `yaml:"siem,omitempty"`
	Admin *AdminConfig `yaml:"admin,omitempty"`
}

// AdminConfig hardens the /api/v1/admin/* surface beyond RBAC — the
// "belt and suspenders" layers. All optional; zero value = RBAC only.
type AdminConfig struct {
	// AllowCIDRs restricts which source IPs may reach admin endpoints.
	// Empty = no IP restriction. Matched against the request's RemoteAddr,
	// so it only protects direct-bind deployments — behind a load balancer
	// RemoteAddr is the LB, so apply IP allow-listing at the ingress there.
	// Also settable via FANTAZM_ADMIN_ALLOW_CIDRS (comma-separated).
	AllowCIDRs []string `yaml:"allowCidrs,omitempty"`
	// RateLimitPerMin caps admin requests per source IP per minute.
	// 0 → the built-in default (60). Negative → disabled.
	RateLimitPerMin int `yaml:"rateLimitPerMin,omitempty"`
	// RequireStepUp gates destructive mutations (role changes, tenant
	// create and settings edits) behind a fresh OIDC login. Supply the
	// single-use, five-minute stepUpToken as X-Step-Up-Token with the same
	// user's bearer token. Requires SSO; ordinary API tokens cannot substitute.
	RequireStepUp bool `yaml:"requireStepUp,omitempty"`
}

type SSOConfig struct {
	RedirectURI  string `yaml:"redirectUri"`
	Issuer       string `yaml:"issuer"`
	ClientID     string `yaml:"clientId"`
	ClientSecret string `yaml:"clientSecret"`
}

type SIEMConfig struct {
	Type     string `yaml:"type"`
	Endpoint string `yaml:"endpoint"`
}

// DefaultConfig returns the secure defaults applied when no config file
// exists or as the base for the merge.
func DefaultConfig() Config {
	return Config{
		Mode: ModePersonal,
		Host: "127.0.0.1",
		Port: 8790,
		Auth: AuthConfig{
			Mode:           AuthModeToken,
			RequireMFA:     false,
			SessionTimeout: 3600 * time.Second,
			DeviceBinding:  true,
		},
		LLM: LLMConfig{
			Provider:    "anthropic",
			Model:       "claude-sonnet-4-5",
			MaxTokens:   4096,
			Temperature: 0.7,
		},
		DataDir:    "./data",
		PolicyFile: "./fathom.policy.yaml",
		LogLevel:   "info",
	}
}

// PermissionSet describes what a session is allowed to do. Sessions inherit
// from the active PolicyConfig defaults unless explicit grants are issued.
type PermissionSet struct {
	Network        string   `yaml:"network" json:"network"`       // "allow" | "deny"
	Filesystem     string   `yaml:"filesystem" json:"filesystem"` // "read-only" | "read-write" | "deny"
	Shell          string   `yaml:"shell" json:"shell"`           // "allow" | "deny"
	Secrets        string   `yaml:"secrets" json:"secrets"`       // "isolated" | "accessible"
	AllowedDomains []string `yaml:"allowedDomains,omitempty" json:"allowedDomains,omitempty"`
	AllowedPaths   []string `yaml:"allowedPaths,omitempty" json:"allowedPaths,omitempty"`
	DeniedPaths    []string `yaml:"deniedPaths,omitempty" json:"deniedPaths,omitempty"`
}

// Session tracks an authenticated caller's interaction lifetime with the gateway.
type Session struct {
	ID          string        `json:"id"`
	UserID      string        `json:"userId"`
	DeviceID    string        `json:"deviceId"`
	CreatedAt   time.Time     `json:"createdAt"`
	ExpiresAt   time.Time     `json:"expiresAt"`
	Permissions PermissionSet `json:"permissions"`
}

// ChannelMessage is the unit of input that flows from a channel adapter
// (CLI, WebSocket, scheduler) into the agent loop.
type ChannelMessage struct {
	ChannelType string    `json:"channelType"`
	ChannelID   string    `json:"channelId"`
	SenderID    string    `json:"senderId"`
	SenderName  string    `json:"senderName,omitempty"`
	Text        string    `json:"text"`
	Timestamp   time.Time `json:"timestamp"`
}

// PolicyConfig is the on-disk shape of fathom.policy.yaml.
type PolicyConfig struct {
	Version  int           `yaml:"version"`
	Defaults PermissionSet `yaml:"defaults"`
	Rules    []PolicyRule  `yaml:"rules"`
}

// PolicyRule expresses a single deny-or-allow decision triggered by a
// matching action/skill/path/domain.
type PolicyRule struct {
	Name  string                 `yaml:"name"`
	When  map[string]interface{} `yaml:"when"`
	Allow map[string]interface{} `yaml:"allow,omitempty"`
	Deny  map[string]interface{} `yaml:"deny,omitempty"`
	Audit *bool                  `yaml:"audit,omitempty"`
	Alert string                 `yaml:"alert,omitempty"`
}

// PolicyDecision is the result of evaluating a PolicyContext against the engine.
type PolicyDecision string

const (
	PolicyAllow    PolicyDecision = "allow"
	PolicyDeny     PolicyDecision = "deny"
	PolicyEscalate PolicyDecision = "escalate"
)

// AuditAction classifies what the AuditLogger is recording.
type AuditAction string

const (
	AuditToolCall        AuditAction = "tool_call"
	AuditSkillExec       AuditAction = "skill_exec"
	AuditLLMRequest      AuditAction = "llm_request"
	AuditLLMResponse     AuditAction = "llm_response"
	AuditAuth            AuditAction = "auth"
	AuditAuthFailure     AuditAction = "auth_failure"
	AuditPolicyDecision  AuditAction = "policy_decision"
	AuditSessionCreate   AuditAction = "session_create"
	AuditSessionDestroy  AuditAction = "session_destroy"
	AuditSkillInstall    AuditAction = "skill_install"
	AuditSkillRemove     AuditAction = "skill_remove"
	AuditCanaryTriggered AuditAction = "canary_triggered"
	AuditAdminAction     AuditAction = "admin_action"
)

// AuditEntry is one row in the hash-chained audit log.
type AuditEntry struct {
	ID           string                 `json:"id"`
	Timestamp    time.Time              `json:"timestamp"`
	SessionID    string                 `json:"sessionId"`
	UserID       string                 `json:"userId"`
	Action       AuditAction            `json:"action"`
	Detail       map[string]interface{} `json:"detail"`
	PolicyResult PolicyDecision         `json:"policyResult"`
	PreviousHash string                 `json:"previousHash"`
	Hash         string                 `json:"hash"`
}

// SkillManifest is the on-disk shape of skill.manifest.yaml.
type SkillManifest struct {
	Name        string   `yaml:"name"`
	Version     string   `yaml:"version"`
	Author      string   `yaml:"author"`
	Signature   string   `yaml:"signature"`
	Description string   `yaml:"description,omitempty"`
	Category    string   `yaml:"category,omitempty"`
	Tags        []string `yaml:"tags,omitempty"`
	// Language selects the subprocess runtime. "node" (default, when
	// empty) loads index.ts via the embedded JS runner; "python" loads
	// the entry module via the embedded runner.py. Same stdin/stdout
	// JSON protocol either way — the skill author's code shape is the
	// only thing that differs.
	Language    string           `yaml:"language,omitempty"`
	Permissions SkillPermissions `yaml:"permissions"`
	Tools       []SkillToolMeta  `yaml:"tools,omitempty"`
}

type SkillPermissions struct {
	Network    []string `yaml:"network,omitempty"`
	Filesystem string   `yaml:"filesystem,omitempty"`
	Shell      string   `yaml:"shell,omitempty"`
	Memory     string   `yaml:"memory,omitempty"`
	Secrets    []string `yaml:"secrets,omitempty"`
}

// SkillToolMeta is one tool exposed by a skill manifest. The host reads this
// to build LLM tool definitions without importing skill code; the actual
// function (named by Function) runs inside the sandboxed subprocess.
type SkillToolMeta struct {
	Name        string                 `yaml:"name"`
	Function    string                 `yaml:"function"`
	Description string                 `yaml:"description"`
	Parameters  map[string]interface{} `yaml:"parameters"`
}
