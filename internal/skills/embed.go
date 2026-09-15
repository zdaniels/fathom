package skills

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zdaniels/fathom/pkg/types"
	"gopkg.in/yaml.v3"
)

// Embedded skill source files. Shipped inside the binary so `fathom install`
// works without the user cloning the repo. The build directive intentionally
// excludes node_modules + any vendored dependencies a future skill might
// pull in — those are downloaded fresh at install time.
//
//go:embed all:bundled
var bundledFS embed.FS

// BundledSkill is the descriptor returned by ListBundled / GetBundled.
type BundledSkill struct {
	Name     string
	Manifest types.SkillManifest
}

// ListBundled enumerates every skill shipped inside the fathom binary.
// Sorted alphabetically for stable output.
func ListBundled() ([]BundledSkill, error) {
	entries, err := bundledFS.ReadDir("bundled")
	if err != nil {
		return nil, fmt.Errorf("read bundled: %w", err)
	}
	var out []BundledSkill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		manifestData, err := bundledFS.ReadFile(filepath.Join("bundled", e.Name(), "skill.manifest.yaml"))
		if err != nil {
			continue // skip dirs without a manifest
		}
		var m types.SkillManifest
		if err := yaml.Unmarshal(manifestData, &m); err != nil {
			continue
		}
		out = append(out, BundledSkill{Name: e.Name(), Manifest: m})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// GetBundled returns the manifest for one bundled skill.
func GetBundled(name string) (*BundledSkill, error) {
	manifestData, err := bundledFS.ReadFile(filepath.Join("bundled", name, "skill.manifest.yaml"))
	if err != nil {
		return nil, fmt.Errorf("skill %q not bundled — run `fathom install` with no args to list available", name)
	}
	var m types.SkillManifest
	if err := yaml.Unmarshal(manifestData, &m); err != nil {
		return nil, fmt.Errorf("parse manifest for %s: %w", name, err)
	}
	return &BundledSkill{Name: name, Manifest: m}, nil
}

// ExtractBundled copies every file from the bundled skill into dest.
// Used by the install command. dest is the SKILL directory (the target
// already includes the skill's name as the last segment).
func ExtractBundled(name, dest string) error {
	src := filepath.Join("bundled", name)
	if _, err := fs.Stat(bundledFS, src); err != nil {
		return fmt.Errorf("skill %q not bundled", name)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	return fs.WalkDir(bundledFS, src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, src)
		rel = strings.TrimPrefix(rel, "/")
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := bundledFS.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
