package security

import (
	"strings"
	"testing"
)

func TestCanaryIssueHasExpectedShape(t *testing.T) {
	c := NewCanarySystem()
	tok := c.Issue("memory:default")
	if !strings.HasPrefix(tok, "ICLW-CANARY-") {
		t.Errorf("token = %q, want prefix ICLW-CANARY-", tok)
	}
	// 12 random bytes → 24 hex chars + 12-char prefix.
	if len(tok) != len("ICLW-CANARY-")+24 {
		t.Errorf("token length = %d, want %d", len(tok), len("ICLW-CANARY-")+24)
	}
}

func TestCanaryDetectsTokenInContent(t *testing.T) {
	c := NewCanarySystem()
	tok := c.Issue("memory:secrets")
	// Embed in plausible LLM output.
	output := "Here's what I found: " + tok + " — let me know!"
	hit := c.Check(output)
	if hit == nil {
		t.Fatal("Check failed to detect canary")
	}
	if hit.Label != "memory:secrets" {
		t.Errorf("label = %q, want 'memory:secrets'", hit.Label)
	}
}

func TestCanaryNoFalsePositiveOnArbitraryText(t *testing.T) {
	c := NewCanarySystem()
	_ = c.Issue("a") // active token in the set
	hit := c.Check("this is a perfectly normal response with no canary tokens in it")
	if hit != nil {
		t.Errorf("false positive: %+v", hit)
	}
}

func TestCanaryRevokeStopsDetection(t *testing.T) {
	c := NewCanarySystem()
	tok := c.Issue("a")
	c.Revoke(tok)
	if c.Check("leak: "+tok) != nil {
		t.Error("revoked token still detected")
	}
}

func TestCanaryMultipleTokensAllDetect(t *testing.T) {
	c := NewCanarySystem()
	tok1 := c.Issue("a")
	tok2 := c.Issue("b")
	if c.Check("contains "+tok1) == nil {
		t.Error("first token undetected")
	}
	if c.Check("contains "+tok2) == nil {
		t.Error("second token undetected")
	}
}
