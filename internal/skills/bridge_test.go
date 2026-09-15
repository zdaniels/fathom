package skills

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zdaniels/fathom/pkg/types"
)

func TestExtractAllowedHostsStripsMethodPrefix(t *testing.T) {
	m := types.SkillManifest{
		Permissions: types.SkillPermissions{
			Network: []string{
				"GET https://api.github.com/*",
				"POST https://api.github.com/*", // dedup
				"PATCH https://api.github.com/issues/*",
			},
		},
	}
	hosts := ExtractAllowedHosts(m)
	if len(hosts) != 1 || hosts[0] != "api.github.com" {
		t.Errorf("hosts = %v, want [api.github.com]", hosts)
	}
}

func TestExtractAllowedHostsMultipleDomains(t *testing.T) {
	m := types.SkillManifest{
		Permissions: types.SkillPermissions{
			Network: []string{
				"GET https://www.googleapis.com/*",
				"POST https://oauth2.googleapis.com/*",
			},
		},
	}
	hosts := ExtractAllowedHosts(m)
	if len(hosts) != 2 {
		t.Errorf("hosts = %v, want 2 distinct", hosts)
	}
}

func TestExtractAllowedHostsBareHostnameFormat(t *testing.T) {
	// Forward-compat: accept "host/path" without METHOD or scheme.
	m := types.SkillManifest{
		Permissions: types.SkillPermissions{
			Network: []string{"api.example.com/v1/*"},
		},
	}
	hosts := ExtractAllowedHosts(m)
	if len(hosts) != 1 || hosts[0] != "api.example.com" {
		t.Errorf("hosts = %v, want [api.example.com]", hosts)
	}
}

func TestHasInstallableSkillsHonoursDirContents(t *testing.T) {
	dir := t.TempDir()
	if HasInstallableSkills(dir) {
		t.Error("empty dir should report no installable skills")
	}
	if HasInstallableSkills(filepath.Join(dir, "nonexistent")) {
		t.Error("missing dir should report no installable skills")
	}
	// One valid skill dir.
	skillDir := filepath.Join(dir, "mySkill")
	_ = os.MkdirAll(skillDir, 0o755)
	_ = os.WriteFile(filepath.Join(skillDir, "skill.manifest.yaml"), []byte("name: x"), 0o644)
	if !HasInstallableSkills(dir) {
		t.Error("dir with a manifest should report installable")
	}
}

func TestStripMethodPrefixHandlesExpected(t *testing.T) {
	cases := map[string]string{
		"GET https://x.com/y":     "https://x.com/y",
		"POST https://x.com/y/*":  "https://x.com/y/*",
		"  PATCH https://x.com/y": "https://x.com/y",
		"https://x.com/y":         "https://x.com/y",
		"x.com/y":                 "x.com/y",
	}
	for in, want := range cases {
		got := stripMethodPrefix(in)
		if got != want {
			t.Errorf("stripMethodPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}
