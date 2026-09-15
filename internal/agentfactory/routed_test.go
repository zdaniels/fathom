package agentfactory

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zdaniels/fathom/internal/skills"
	"github.com/zdaniels/fathom/pkg/types"
)

// writeSkill drops a minimal manifest at dir/<name>/skill.manifest.yaml so
// the bridge / category scanner finds it.
func writeSkill(t *testing.T, dir, name, category string) {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "name: " + name + "\n" +
		"version: \"1.0.0\"\n" +
		"author: test\n" +
		"signature: ed25519:builtin\n" +
		"description: test fixture\n" +
		"permissions: {filesystem: none, shell: none, memory: own}\n"
	if category != "" {
		manifest += "category: " + category + "\n"
	}
	manifest += "tools:\n  - {name: noop, function: noop, description: x, parameters: {type: object, properties: {}}}\n"
	if err := os.WriteFile(filepath.Join(skillDir, "skill.manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveProfileSkills_ExplicitWhitelist(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "alpha", "communication")
	writeSkill(t, dir, "beta", "development")
	writeSkill(t, dir, "gamma", "communication")

	profile := types.ProfileConfig{Skills: []string{"alpha", "gamma"}}
	got := resolveProfileSkills(profile, dir)
	if !equalSorted(got, []string{"alpha", "gamma"}) {
		t.Fatalf("want [alpha gamma], got %v", got)
	}
}

func TestResolveProfileSkills_CategoryExpansion(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "gmail", "communication")
	writeSkill(t, dir, "slack", "communication")
	writeSkill(t, dir, "shell", "development")
	writeSkill(t, dir, "uncategorised", "")

	profile := types.ProfileConfig{Categories: []string{"communication"}}
	got := resolveProfileSkills(profile, dir)
	if !equalSorted(got, []string{"gmail", "slack"}) {
		t.Fatalf("want [gmail slack], got %v", got)
	}
}

func TestResolveProfileSkills_UnionsBoth(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "gmail", "communication")
	writeSkill(t, dir, "shell", "development")
	writeSkill(t, dir, "extra", "other")

	profile := types.ProfileConfig{
		Skills:     []string{"extra"},
		Categories: []string{"communication"},
	}
	got := resolveProfileSkills(profile, dir)
	if !equalSorted(got, []string{"extra", "gmail"}) {
		t.Fatalf("want [extra gmail], got %v", got)
	}
}

func TestResolveProfileSkills_CategoryCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "gmail", "Communication") // capital-C
	profile := types.ProfileConfig{Categories: []string{"communication"}}
	got := resolveProfileSkills(profile, dir)
	if !equalSorted(got, []string{"gmail"}) {
		t.Fatalf("category match should be case-insensitive; got %v", got)
	}
}

func TestResolveProfileSkills_EmptyProfileReturnsEmptyWhitelist(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "gmail", "communication")
	profile := types.ProfileConfig{}
	got := resolveProfileSkills(profile, dir)
	if got == nil {
		t.Fatal("empty profile should return non-nil empty slice (explicit empty whitelist), not nil (which means 'all')")
	}
	if len(got) != 0 {
		t.Fatalf("want empty slice, got %v", got)
	}
}

func TestProfilesNeedSkills(t *testing.T) {
	cases := []struct {
		name string
		cfg  *types.RoutingConfig
		want bool
	}{
		{"nil", nil, false},
		{"empty profiles", &types.RoutingConfig{}, false},
		{"profile with skills", &types.RoutingConfig{Profiles: map[string]types.ProfileConfig{
			"x": {Skills: []string{"a"}},
		}}, true},
		{"profile with categories", &types.RoutingConfig{Profiles: map[string]types.ProfileConfig{
			"x": {Categories: []string{"comm"}},
		}}, true},
		{"profile with neither", &types.RoutingConfig{Profiles: map[string]types.ProfileConfig{
			"x": {},
		}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := profilesNeedSkills(c.cfg); got != c.want {
				t.Errorf("want %v, got %v", c.want, got)
			}
		})
	}
}

// Integration-ish: verify that skills.Bridge actually honours the Only
// filter when called the way routed.go calls it. This is more of a
// regression guard than a pure unit test — it depends on the real
// skills.Bridge but with no egress / vault, the bridge skips actual
// installs and we just verify the filter math.
func TestBridgeFilter_OnlyIncludesNamedSkills(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "gmail", "communication")
	writeSkill(t, dir, "shell", "development")
	writeSkill(t, dir, "search", "other")

	if !skills.HasInstallableSkills(dir) {
		t.Fatal("manifests should be installable; HasInstallableSkills returned false")
	}

	categories := map[string]bool{"communication": true}
	got := skills.SkillsByCategory(dir, categories)
	if !equalSorted(got, []string{"gmail"}) {
		t.Fatalf("want [gmail], got %v", got)
	}
}

func TestPickRuntimeByModel(t *testing.T) {
	runtimes := map[string]*profileRuntime{
		"general": {name: "general", modelDesc: "qwen3:14b"},
		"code":    {name: "code", modelDesc: "qwen2.5-coder:14b"},
	}
	order := []string{"code", "general"}
	if got := pickRuntimeByModel(runtimes, order, "qwen2.5-coder:14b"); got == nil || got.name != "code" {
		t.Fatalf("want code, got %+v", got)
	}
	if got := pickRuntimeByModel(runtimes, order, "nope"); got != nil {
		t.Fatalf("unknown model should return nil, got %+v", got)
	}
}

// equalSorted returns true if a and b contain the same elements (order
// independent). Used for whitelist comparisons.
func equalSorted(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, x := range a {
		seen[x]++
	}
	for _, x := range b {
		seen[x]--
	}
	for _, v := range seen {
		if v != 0 {
			return false
		}
	}
	return true
}
