package security

import (
	"sync"
)

// CanarySystem generates honeypot strings ("canary tokens"), seeds them into
// memory + context, and detects them in LLM output. If a canary ever shows
// up in a tool call's output or in the model's response, that's evidence
// of exfiltration — Fathom flags the session and refuses to forward the
// content to the user.
//
// The tokens are 16-byte hex strings prefixed with "ICLW-CANARY-" — short
// enough to inject inline, unique enough to give effectively zero false
// positives on real content.
type CanarySystem struct {
	mu     sync.Mutex
	tokens map[string]CanaryToken
}

// CanaryToken is one labelled honeypot.
type CanaryToken struct {
	Token string
	Label string
}

// NewCanarySystem starts empty. Tokens get added via Issue.
func NewCanarySystem() *CanarySystem {
	return &CanarySystem{tokens: make(map[string]CanaryToken)}
}

// Issue returns a new canary token labelled with `label`. The label is what
// gets logged when the canary fires — e.g. "memory:secrets-namespace".
func (c *CanarySystem) Issue(label string) string {
	b, _ := RandomBytes(12)
	tok := "ICLW-CANARY-" + hexEncode(b)
	c.mu.Lock()
	c.tokens[tok] = CanaryToken{Token: tok, Label: label}
	c.mu.Unlock()
	return tok
}

// Check scans content for any issued canary. Returns the first match.
// Returns nil if nothing tripped — the common case in production.
func (c *CanarySystem) Check(content string) *CanaryToken {
	c.mu.Lock()
	defer c.mu.Unlock()
	for tok, info := range c.tokens {
		if containsString(content, tok) {
			info := info
			return &info
		}
	}
	return nil
}

// Revoke removes a token from the active set. Used after rotation.
func (c *CanarySystem) Revoke(token string) {
	c.mu.Lock()
	delete(c.tokens, token)
	c.mu.Unlock()
}

func hexEncode(b []byte) string {
	const alphabet = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = alphabet[v>>4]
		out[i*2+1] = alphabet[v&0x0f]
	}
	return string(out)
}

func containsString(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
