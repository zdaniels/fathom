package security

import (
	"github.com/zdaniels/fathom/pkg/types"
)

// Mesh aggregates the security layers Fathom layers in front of every agent
// invocation. Single struct so consumers (the agent loop, tool registry,
// gateway, mountEnterprise) pass one thing around instead of six.
type Mesh struct {
	Sanitizer   *Sanitizer
	Policy      *PolicyEngine
	Audit       *AuditLogger
	RateLimiter *RateLimiter
	Secrets     *SecretsVault
	Canary      *CanarySystem
}

// MeshOptions allows callers to override pieces — agent-factory passes its
// pre-opened vault here so the mesh and the LLM key resolver share state.
type MeshOptions struct {
	Secrets *SecretsVault
}

// NewMesh constructs a Mesh from a policy config + options. Each component
// is independently usable; Mesh is a convenience for "everything in one place".
func NewMesh(policy types.PolicyConfig, opts MeshOptions) *Mesh {
	m := &Mesh{
		Sanitizer:   NewSanitizer(),
		Policy:      NewPolicyEngine(policy),
		Audit:       NewAuditLogger(10_000),
		RateLimiter: NewRateLimiter(),
		Canary:      NewCanarySystem(),
	}
	if opts.Secrets != nil {
		m.Secrets = opts.Secrets
	}
	return m
}
