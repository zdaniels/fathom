package security

import (
	"testing"

	"github.com/zdaniels/fathom/pkg/types"
)

func denyAllPolicy() *PolicyEngine {
	return NewPolicyEngine(types.PolicyConfig{
		Version: 1,
		Defaults: types.PermissionSet{
			Network: "deny", Filesystem: "deny", Shell: "deny", Secrets: "isolated",
		},
	})
}

func TestPolicyAllowsToolCallByDefault(t *testing.T) {
	// applyDefaults only denies when the contextual field that's denied is
	// actually populated. A bare action="tool:..." has no domain/path/command,
	// so deny-all defaults don't fire — the tool runs.
	p := denyAllPolicy()
	got := p.Evaluate(PolicyContext{Action: "tool:web_search"})
	if got.Decision != types.PolicyAllow {
		t.Errorf("bare tool call decision = %v, want allow", got.Decision)
	}
}

func TestPolicyDeniesShellExecByDefault(t *testing.T) {
	p := denyAllPolicy()
	got := p.Evaluate(PolicyContext{Action: "shell-exec"})
	if got.Decision != types.PolicyDeny {
		t.Errorf("shell-exec decision = %v, want deny", got.Decision)
	}
}

func TestPolicyDeniesNetworkByDefault(t *testing.T) {
	p := denyAllPolicy()
	got := p.Evaluate(PolicyContext{Domain: "evil.example.com"})
	if got.Decision != types.PolicyDeny {
		t.Errorf("network decision = %v, want deny", got.Decision)
	}
}

func TestPolicyExplicitDenyRuleWinsOverDefaults(t *testing.T) {
	// Use `**` because single `*` doesn't span path separators (matches the
	// TS impl). A real-world block-rm rule would write `rm **` or list each
	// shape it wants to block.
	rule := types.PolicyRule{
		Name: "block-shell-rm",
		When: map[string]interface{}{"action": "shell-exec"},
		Deny: map[string]interface{}{"command": "rm **"},
	}
	p := NewPolicyEngine(types.PolicyConfig{
		Defaults: types.PermissionSet{Network: "allow", Filesystem: "read-write", Shell: "allow", Secrets: "accessible"},
		Rules:    []types.PolicyRule{rule},
	})
	got := p.Evaluate(PolicyContext{Action: "shell-exec", Command: "rm /tmp/x"})
	if got.Decision != types.PolicyDeny || got.MatchedRule != "block-shell-rm" {
		t.Errorf("rule deny: decision=%v rule=%q", got.Decision, got.MatchedRule)
	}
}

func TestPolicyAllowRuleApplies(t *testing.T) {
	rule := types.PolicyRule{
		Name:  "open-github",
		When:  map[string]interface{}{"action": "tool:network"},
		Allow: map[string]interface{}{"network": "api.github.com"},
	}
	p := NewPolicyEngine(types.PolicyConfig{
		Defaults: types.PermissionSet{Network: "deny"},
		Rules:    []types.PolicyRule{rule},
	})
	got := p.Evaluate(PolicyContext{Action: "tool:network", Domain: "api.github.com"})
	if got.Decision != types.PolicyAllow {
		t.Errorf("decision = %v, want allow", got.Decision)
	}
}

func TestPolicyCheckNetworkHonoursDefaultDeny(t *testing.T) {
	p := denyAllPolicy() // defaults: network=deny
	dec := p.CheckNetwork("api.github.com")
	if dec.Decision != types.PolicyDeny {
		t.Errorf("CheckNetwork under deny-default = %v, want deny", dec.Decision)
	}
}

func TestPolicyCheckNetworkAllowsByDefault(t *testing.T) {
	p := NewPolicyEngine(types.PolicyConfig{
		Defaults: types.PermissionSet{Network: "allow", Filesystem: "read-write", Shell: "deny"},
	})
	dec := p.CheckNetwork("api.github.com")
	if dec.Decision != types.PolicyAllow {
		t.Errorf("CheckNetwork under allow-default = %v, want allow", dec.Decision)
	}
}

func TestPolicyCheckFilesystemDeniesWriteUnderReadOnly(t *testing.T) {
	p := NewPolicyEngine(types.PolicyConfig{
		Defaults: types.PermissionSet{Network: "allow", Filesystem: "read-only", Shell: "deny"},
	})
	if dec := p.CheckFilesystem("/tmp/x", "write"); dec.Decision != types.PolicyDeny {
		t.Errorf("write under read-only = %v, want deny", dec.Decision)
	}
	if dec := p.CheckFilesystem("/tmp/x", "read"); dec.Decision != types.PolicyAllow {
		t.Errorf("read under read-only = %v, want allow", dec.Decision)
	}
}

func TestPolicyCheckFilesystemDeniesBothUnderDeny(t *testing.T) {
	p := denyAllPolicy()
	if dec := p.CheckFilesystem("/tmp/x", "read"); dec.Decision != types.PolicyDeny {
		t.Errorf("read under deny = %v, want deny", dec.Decision)
	}
	if dec := p.CheckFilesystem("/tmp/x", "write"); dec.Decision != types.PolicyDeny {
		t.Errorf("write under deny = %v, want deny", dec.Decision)
	}
}

func TestPolicyGlobMatchSingleSegment(t *testing.T) {
	cases := []struct {
		pattern, value string
		want           bool
	}{
		{"/etc/*.conf", "/etc/foo.conf", true},
		{"/etc/*.conf", "/etc/sub/foo.conf", false},
		{"/etc/**/foo", "/etc/a/b/foo", true},
		{"/etc/**/foo", "/etc/foo", false},
		{"exact", "exact", true},
		{"a", "b", false},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.value); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.value, got, c.want)
		}
	}
}
