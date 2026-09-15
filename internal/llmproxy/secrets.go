package llmproxy

import (
	"fmt"
	"os"
)

// SecretResolver is what the proxy uses to turn a SecretRef into the
// actual value. Same shape as llm.SecretLookup but takes a SecretRef so
// the caller doesn't have to flatten {vault, env, literal} themselves.
//
// Production setups wire this to the fathom vault. Tests can hand in a
// trivial in-memory resolver. The literal case is supported here so
// smoke tests + dev configs don't have to bring up infrastructure.
type SecretResolver func(SecretRef) (string, error)

// EnvResolver reads SecretRef.Env or .Literal only — used by tests and
// configs that don't want a vault on the path. Returns an error if a
// vault-backed ref is encountered, so we fail loud instead of silently
// returning empty strings on misconfiguration.
func EnvResolver(ref SecretRef) (string, error) {
	if ref.Literal != "" {
		return ref.Literal, nil
	}
	if ref.Env != "" {
		v := os.Getenv(ref.Env)
		if v == "" {
			return "", fmt.Errorf("env %s is not set", ref.Env)
		}
		return v, nil
	}
	if ref.Vault != "" {
		return "", fmt.Errorf("vault-backed secret %q requested but no vault resolver configured", ref.Vault)
	}
	return "", fmt.Errorf("empty SecretRef")
}
