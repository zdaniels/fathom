package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zdaniels/fathom/pkg/types"
)

func TestLoadConfigUsesDefaultsWhenFileMissing(t *testing.T) {
	cfg := LoadConfig(filepath.Join(t.TempDir(), "no-such-file.yaml"))
	if cfg.Mode != types.ModePersonal {
		t.Errorf("default mode = %q, want personal", cfg.Mode)
	}
	if cfg.Port != 8790 {
		t.Errorf("default port = %d, want 8790", cfg.Port)
	}
	if cfg.LLM.Provider != "anthropic" {
		t.Errorf("default provider = %q, want anthropic", cfg.LLM.Provider)
	}
}

func TestLoadConfigOverlaysYAMLOnDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fathom.config.yaml")
	yaml := `mode: team
port: 9090
llm:
  provider: openai
  model: gpt-4o
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := LoadConfig(path)
	if cfg.Mode != types.ModeTeam {
		t.Errorf("mode = %q, want team", cfg.Mode)
	}
	if cfg.Port != 9090 {
		t.Errorf("port = %d, want 9090", cfg.Port)
	}
	if cfg.LLM.Provider != "openai" || cfg.LLM.Model != "gpt-4o" {
		t.Errorf("llm = %+v, want openai/gpt-4o", cfg.LLM)
	}
	// Untouched defaults should still be there.
	if cfg.Host != "127.0.0.1" {
		t.Errorf("host = %q, want 127.0.0.1 (default)", cfg.Host)
	}
}

func TestFANTAZM_MODEEnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fathom.config.yaml")
	os.WriteFile(path, []byte("mode: personal\n"), 0o644)

	t.Setenv("FANTAZM_MODE", "enterprise")
	cfg := LoadConfig(path)
	if cfg.Mode != types.ModeEnterprise {
		t.Errorf("env override failed: mode = %q, want enterprise", cfg.Mode)
	}
}

func TestInvalidFANTAZM_MODEFallsThrough(t *testing.T) {
	t.Setenv("FANTAZM_MODE", "bogus")
	cfg := LoadConfig(filepath.Join(t.TempDir(), "x.yaml"))
	if cfg.Mode != types.ModePersonal {
		t.Errorf("invalid env should fall to default: mode = %q", cfg.Mode)
	}
}

func TestLoadPolicyDefaultsToDenyAll(t *testing.T) {
	pol := LoadPolicy(filepath.Join(t.TempDir(), "missing.yaml"))
	if pol.Defaults.Network != "deny" {
		t.Errorf("default network = %q, want deny", pol.Defaults.Network)
	}
	if pol.Defaults.Shell != "deny" {
		t.Errorf("default shell = %q, want deny", pol.Defaults.Shell)
	}
}

func TestValidationClampsSessionTimeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fathom.config.yaml")
	os.WriteFile(path, []byte("auth:\n  sessionTimeout: 5s\n"), 0o644)
	cfg := LoadConfig(path)
	if cfg.Auth.SessionTimeout.Seconds() < 60 {
		t.Errorf("session timeout not clamped: %v", cfg.Auth.SessionTimeout)
	}
}
