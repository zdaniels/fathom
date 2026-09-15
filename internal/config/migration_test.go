package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyConfigAndPolicyDiscovery(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("FATHOM_CONFIG", "")
	t.Setenv("FATHOM_POLICY", "")
	for _, name := range []string{"config", "policy"} {
		legacy := filepath.Join(dir, "fantazm."+name+".yaml")
		current := filepath.Join(dir, "fathom."+name+".yaml")
		discover := DiscoverConfig
		if name == "policy" {
			discover = DiscoverPolicy
		}
		if err := os.WriteFile(legacy, []byte(""), 0600); err != nil {
			t.Fatal(err)
		}
		if got := discover(); got != legacy {
			t.Fatalf("legacy %s: got %q", name, got)
		}
		if err := os.WriteFile(current, []byte(""), 0600); err != nil {
			t.Fatal(err)
		}
		if got := discover(); got != current {
			t.Fatalf("current %s: got %q", name, got)
		}
	}
}

func TestFathomEnvOverridesLegacy(t *testing.T) {
	t.Setenv("FANTAZM_PORT", "9001")
	t.Setenv("FATHOM_PORT", "9002")
	cfg := LoadConfig(filepath.Join(t.TempDir(), "missing.yaml"))
	if cfg.Port != 9002 {
		t.Fatalf("port = %d, want 9002", cfg.Port)
	}
}
