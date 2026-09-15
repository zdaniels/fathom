package security

import (
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/zdaniels/fathom/pkg/types"
)

// PolicyEngine evaluates per-action policy decisions against the loaded
// rule set + deny-by-default defaults. Same matching semantics as the TS
// implementation: rules iterate in order, first match wins; if nothing
// matches, fall back to applyDefaults which checks the context's specific
// fields (action, domain, path) against the defaults' deny entries.
type PolicyEngine struct {
	// mu guards config so the settings API can hot-swap the rule set
	// (Reload) while agent turns are concurrently calling Evaluate. The
	// lock is held only for the pointer read/write, not across the match
	// loop — Evaluate snapshots the config under RLock then matches on
	// the snapshot, so a reload mid-evaluation just means the in-flight
	// call finishes against the pre-reload rules (the next call sees new).
	mu     sync.RWMutex
	config types.PolicyConfig
}

// PolicyContext is what a tool/skill invocation feeds into the engine.
type PolicyContext struct {
	Skill             string
	Action            string
	Path              string
	Command           string
	Domain            string
	Op                string // "read" | "write" | "delete" — for filesystem ops
	UserID            string
	SessionPermission types.PermissionSet
}

// PolicyEvaluation is the engine's output — what to do + why.
type PolicyEvaluation struct {
	Decision    types.PolicyDecision
	MatchedRule string
	Reason      string
	Alert       string
	Audit       bool
}

// NewPolicyEngine constructs from a parsed PolicyConfig (LoadPolicy in
// internal/config produces these).
func NewPolicyEngine(cfg types.PolicyConfig) *PolicyEngine {
	return &PolicyEngine{config: cfg}
}

// Evaluate runs the rules against ctx. Returns the first decisive match
// (deny wins) or falls through to defaults.
func (p *PolicyEngine) Evaluate(ctx PolicyContext) PolicyEvaluation {
	cfg := p.snapshot()
	for _, rule := range cfg.Rules {
		if !matchesWhen(rule.When, ctx) {
			continue
		}
		if rule.Deny != nil && matchesConstraints(rule.Deny, ctx) {
			audit := true
			if rule.Audit != nil {
				audit = *rule.Audit
			}
			return PolicyEvaluation{
				Decision:    types.PolicyDeny,
				MatchedRule: rule.Name,
				Reason:      "Denied by rule: " + rule.Name,
				Alert:       rule.Alert,
				Audit:       audit,
			}
		}
		if rule.Allow != nil && matchesConstraints(rule.Allow, ctx) {
			audit := false
			if rule.Audit != nil {
				audit = *rule.Audit
			}
			return PolicyEvaluation{
				Decision:    types.PolicyAllow,
				MatchedRule: rule.Name,
				Reason:      "Allowed by rule: " + rule.Name,
				Audit:       audit,
			}
		}
	}
	return applyDefaults(cfg, ctx)
}

// snapshot returns the current config under a read lock. Callers iterate
// the returned value, so a concurrent Reload can't tear the slice.
func (p *PolicyEngine) snapshot() types.PolicyConfig {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.config
}

// Snapshot returns a copy of the live policy config. The settings API
// reads this to render the current rules/defaults to the admin UI.
func (p *PolicyEngine) Snapshot() types.PolicyConfig {
	return p.snapshot()
}

// Reload atomically swaps the rule set. The settings API calls this after
// persisting fathom.policy.yaml so policy changes take effect without a
// gateway restart. Tools hold the engine by pointer, so every subsequent
// Evaluate sees the new rules.
func (p *PolicyEngine) Reload(cfg types.PolicyConfig) {
	p.mu.Lock()
	p.config = cfg
	p.mu.Unlock()
}

// applyDefaults turns the engine's "no rule matched" path into a decision
// based on the contextual fields and the policy defaults.
func applyDefaults(cfg types.PolicyConfig, ctx PolicyContext) PolicyEvaluation {
	d := cfg.Defaults
	if ctx.Action == "shell-exec" && d.Shell == "deny" {
		return PolicyEvaluation{Decision: types.PolicyDeny, Reason: "Shell access denied by default", Audit: true}
	}
	if ctx.Domain != "" && d.Network == "deny" {
		return PolicyEvaluation{Decision: types.PolicyDeny, Reason: "Network access denied by default", Audit: true}
	}
	if ctx.Path != "" {
		switch d.Filesystem {
		case "deny":
			return PolicyEvaluation{Decision: types.PolicyDeny, Reason: "Filesystem access denied by default", Audit: true}
		case "read-only":
			if ctx.Op == "write" || ctx.Op == "delete" {
				return PolicyEvaluation{Decision: types.PolicyDeny, Reason: "Filesystem is read-only by default", Audit: true}
			}
		}
	}
	return PolicyEvaluation{Decision: types.PolicyAllow, Reason: "Allowed by default policy", Audit: false}
}

// CheckNetwork is the tool-side helper for "I'm about to hit domain X".
// Tools call this BEFORE the actual HTTP request — denial blocks the egress.
func (p *PolicyEngine) CheckNetwork(domain string) PolicyEvaluation {
	return p.Evaluate(PolicyContext{Action: "network", Domain: domain})
}

// CheckFilesystem is the tool-side helper for "I'm about to read/write a
// path." op is "read", "write", or "delete". Tools call this BEFORE the I/O.
func (p *PolicyEngine) CheckFilesystem(path, op string) PolicyEvaluation {
	return p.Evaluate(PolicyContext{Action: "filesystem", Path: path, Op: op})
}

// matchesWhen is true iff every populated field in the rule's "when" matches ctx.
func matchesWhen(when map[string]interface{}, ctx PolicyContext) bool {
	if when == nil {
		return true
	}
	if s, ok := when["skill"].(string); ok && ctx.Skill != s {
		return false
	}
	if s, ok := when["action"].(string); ok && ctx.Action != s {
		return false
	}
	if s, ok := when["path"].(string); ok && ctx.Path != "" {
		if matched, _ := filepath.Match(s, ctx.Path); !matched {
			return false
		}
	}
	return true
}

// matchesConstraints is true iff any pattern in any constraint key matches
// the corresponding context value. Allow constraints reuse the same matcher
// as deny — semantics: "if any of these patterns describe me, the verdict
// applies." Network constraint compares against ctx.Domain, filesystem
// against ctx.Path, command against ctx.Command.
func matchesConstraints(constraints map[string]interface{}, ctx PolicyContext) bool {
	for key, patterns := range constraints {
		val := contextValue(key, ctx)
		if val == "" {
			continue
		}
		list := toStringSlice(patterns)
		for _, pat := range list {
			if globMatch(pat, val) {
				return true
			}
		}
	}
	return false
}

func contextValue(key string, ctx PolicyContext) string {
	switch key {
	case "network":
		return ctx.Domain
	case "filesystem", "path":
		return ctx.Path
	case "command":
		return ctx.Command
	default:
		return ""
	}
}

func toStringSlice(v interface{}) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	}
	return nil
}

// globMatch matches Unix-glob style with the same semantics as the TS impl:
//
//   - matches any sequence of chars EXCEPT path separator (one segment)
//     **  matches any sequence including separators (cross-segment)
//     ?   matches a single non-separator char
//
// Implemented by translating to a regexp; small enough that the compile cost
// is dwarfed by the actual evaluation work and the lifetime of a policy
// engine is the whole gateway, so there's no per-eval allocation overhead
// in practice. (We could cache compiled regexps per pattern if profiling
// ever shows it.)
func globMatch(pattern, value string) bool {
	if pattern == value {
		return true
	}
	re, err := compileGlob(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(value)
}

func compileGlob(pattern string) (*regexp.Regexp, error) {
	var sb strings.Builder
	sb.WriteByte('^')
	i := 0
	for i < len(pattern) {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				sb.WriteString(".*") // ** spans path separators
				i += 2
			} else {
				sb.WriteString("[^/]*") // single * stays within a segment
				i++
			}
		case '?':
			sb.WriteString("[^/]")
			i++
		case '.', '+', '(', ')', '|', '[', ']', '{', '}', '^', '$', '\\':
			sb.WriteByte('\\')
			sb.WriteByte(pattern[i])
			i++
		default:
			sb.WriteByte(pattern[i])
			i++
		}
	}
	sb.WriteByte('$')
	return regexp.Compile(sb.String())
}
