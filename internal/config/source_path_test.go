package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExplicitConfigRetainsPolicyAndSettingsPath(t *testing.T) {
	dir := t.TempDir()
	explicit := filepath.Join(dir, "chosen.yaml")
	if err := os.WriteFile(explicit, []byte("policyFile: ./policy.yaml\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FATHOM_CONFIG", filepath.Join(t.TempDir(), "other.yaml"))
	cfg := LoadConfig(explicit)
	t.Chdir(t.TempDir())
	if cfg.ConfigPath != explicit {
		t.Fatal(cfg.ConfigPath)
	}
	if got := ResolvePolicyPath(cfg); got != filepath.Join(dir, "policy.yaml") {
		t.Fatalf("policy rediscovered: %s", got)
	}
}
