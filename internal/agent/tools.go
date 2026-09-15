package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/zdaniels/fathom/internal/agent/llm"
	"github.com/zdaniels/fathom/internal/security"
	"github.com/zdaniels/fathom/pkg/types"
)

// ToolContext is what an in-process ToolDefinition.Execute receives. Sandboxed
// skills (subprocess) receive a similar shape via the runner's stdin JSON.
type ToolContext struct {
	SessionID   string
	UserID      string
	Permissions types.PermissionSet

	// GetSecret resolves a secret scoped to this tool's owning skill. The
	// ToolRegistry binds the skill name when populating this on a per-call
	// basis — the tool just calls GetSecret(name).
	GetSecret func(name string) (string, error)

	// Policy is the security mesh's policy engine. Tools that perform
	// network or filesystem operations MUST call ctx.Policy.CheckNetwork or
	// ctx.Policy.CheckFilesystem before the side-effect — that's how the
	// policy file's defaults (network: deny, filesystem: read-only, etc.)
	// actually get enforced for tool calls. The outer registry-level
	// Evaluate only checks the per-tool "tool:NAME" action.
	Policy *security.PolicyEngine
}

// ToolDefinition is an in-process tool registered with the agent. Sandboxed
// skills register a thin wrapper whose Execute spawns the subprocess.
type ToolDefinition struct {
	Name        string
	Description string
	Parameters  map[string]interface{}

	// SkillName scopes vault secret lookups. Built-in tools (notes,
	// web-search, file-editor) are tagged with their skill name so a tool
	// in "github" can't read a secret scoped to "gmail".
	SkillName string

	Execute func(ctx context.Context, params map[string]interface{}, tctx ToolContext) (interface{}, error)
}

// SecretResolver is the host-side function the registry uses to populate
// ToolContext.GetSecret for each invocation.
type SecretResolver func(name, requestingSkill string) (string, error)

// ToolResult is the structured outcome of a single tool execution.
type ToolResult struct {
	CallID     string
	Success    bool
	Output     interface{}
	Error      string
	DurationMs int64
}

// ToolRegistry holds the agent's tool set + the bindings that turn an LLM
// tool call into a Go function call.
type ToolRegistry struct {
	mu             sync.RWMutex
	tools          map[string]ToolDefinition
	policy         *security.PolicyEngine
	audit          *security.AuditLogger
	canary         *security.CanarySystem
	secretResolver SecretResolver
}

// NewToolRegistry assembles a registry wired to the security mesh.
func NewToolRegistry(policy *security.PolicyEngine, audit *security.AuditLogger, canary *security.CanarySystem) *ToolRegistry {
	return &ToolRegistry{
		tools:  make(map[string]ToolDefinition),
		policy: policy,
		audit:  audit,
		canary: canary,
	}
}

// Register adds a tool by name. Idempotent — re-registering overwrites.
func (r *ToolRegistry) Register(t ToolDefinition) {
	r.mu.Lock()
	r.tools[t.Name] = t
	r.mu.Unlock()
	slog.Info("tool registered", "name", t.Name, "skill", orDefault(t.SkillName, "(host)"))
}

// SetSecretResolver wires the host's secret-lookup function. The registry
// binds the calling tool's skill name to the resolver on each invocation.
func (r *ToolRegistry) SetSecretResolver(fn SecretResolver) {
	r.mu.Lock()
	r.secretResolver = fn
	r.mu.Unlock()
}

// LLMDefs returns the JSON-shaped tool definitions to ship to the LLM.
func (r *ToolRegistry) LLMDefs() []llm.ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]llm.ToolDef, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, llm.ToolDef{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	return out
}

// Names returns the list of registered tool names — used for the boot banner.
func (r *ToolRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.tools))
	for n := range r.tools {
		out = append(out, n)
	}
	return out
}

// Execute runs a tool call against the registry, recording audit entries,
// applying policy, and running canary checks on the output. Returns a
// ToolResult — never panics; tool errors land in Error with Success=false.
func (r *ToolRegistry) Execute(ctx context.Context, call llm.ToolCallRequest, session types.Session) ToolResult {
	start := time.Now()
	r.mu.RLock()
	tool, ok := r.tools[call.Name]
	resolver := r.secretResolver
	r.mu.RUnlock()
	if !ok {
		return ToolResult{CallID: call.ID, Success: false, Error: "unknown tool: " + call.Name,
			DurationMs: time.Since(start).Milliseconds()}
	}

	// Policy + audit before invocation.
	policyEval := r.policy.Evaluate(security.PolicyContext{
		Action:            "tool:" + call.Name,
		UserID:            session.UserID,
		SessionPermission: session.Permissions,
	})
	r.audit.Log(session.ID, session.UserID, types.AuditToolCall, map[string]interface{}{
		"tool":   call.Name,
		"params": sanitizeParamsForLog(call.Arguments),
	}, policyEval.Decision)
	if policyEval.Decision == types.PolicyDeny {
		slog.Warn("tool call denied by policy", "tool", call.Name, "rule", policyEval.MatchedRule)
		return ToolResult{CallID: call.ID, Success: false,
			Error:      "Policy denied: " + policyEval.Reason,
			DurationMs: time.Since(start).Milliseconds()}
	}

	// Bind the secret resolver to this tool's skill name so vault scope
	// is enforced automatically.
	var getSecret func(name string) (string, error)
	if resolver != nil {
		skill := tool.SkillName
		getSecret = func(name string) (string, error) { return resolver(name, skill) }
	}
	tctx := ToolContext{
		SessionID:   session.ID,
		UserID:      session.UserID,
		Permissions: session.Permissions,
		GetSecret:   getSecret,
		Policy:      r.policy,
	}

	output, err := tool.Execute(ctx, call.Arguments, tctx)
	if err != nil {
		return ToolResult{CallID: call.ID, Success: false, Error: err.Error(),
			DurationMs: time.Since(start).Milliseconds()}
	}

	outStr := ""
	switch v := output.(type) {
	case string:
		outStr = v
	default:
		b, _ := json.Marshal(output)
		outStr = string(b)
	}
	if canTrip := r.canary.Check(outStr); canTrip != nil {
		r.audit.Log(session.ID, session.UserID, types.AuditCanaryTriggered, map[string]interface{}{
			"tool":  call.Name,
			"label": canTrip.Label,
		}, types.PolicyDeny)
		return ToolResult{CallID: call.ID, Success: false,
			Error:      "Security alert: suspicious data access detected",
			DurationMs: time.Since(start).Milliseconds()}
	}

	return ToolResult{CallID: call.ID, Success: true, Output: output,
		DurationMs: time.Since(start).Milliseconds()}
}

var sensitiveParamKey = regexp.MustCompile(`(?i)key|secret|password|token|auth`)

func sanitizeParamsForLog(params map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(params))
	for k, v := range params {
		if sensitiveParamKey.MatchString(k) {
			out[k] = "[REDACTED]"
			continue
		}
		if s, ok := v.(string); ok && len(s) > 500 {
			out[k] = s[:500] + "...[truncated]"
			continue
		}
		out[k] = v
	}
	return out
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
