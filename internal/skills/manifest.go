package skills

import (
	"fmt"
	"os"
	"regexp"

	"github.com/zdaniels/fathom/pkg/types"
	"gopkg.in/yaml.v3"
)

// LoadManifest reads and validates skill.manifest.yaml.
func LoadManifest(path string) (types.SkillManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return types.SkillManifest{}, err
	}
	return ParseManifest(data)
}

// ParseManifest validates the yaml bytes against the schema.
func ParseManifest(data []byte) (types.SkillManifest, error) {
	var m types.SkillManifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return types.SkillManifest{}, fmt.Errorf("manifest parse: %w", err)
	}
	if err := ValidateManifest(m); err != nil {
		return types.SkillManifest{}, err
	}
	return m, nil
}

var (
	skillNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	semverRE    = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	networkRE   = regexp.MustCompile(`^(GET|POST|PUT|DELETE|PATCH|HEAD|\*)\s+https?://`)
)

// ValidateManifest enforces the same shape rules as the TS impl.
func ValidateManifest(m types.SkillManifest) error {
	if m.Name == "" {
		return fmt.Errorf("missing required field: name")
	}
	if !skillNameRE.MatchString(m.Name) {
		return fmt.Errorf("invalid skill name %q: must be lowercase alphanumeric with hyphens", m.Name)
	}
	if m.Version == "" || !semverRE.MatchString(m.Version) {
		return fmt.Errorf("invalid version %q: must be semver (x.y.z)", m.Version)
	}
	if m.Author == "" {
		return fmt.Errorf("missing required field: author")
	}
	if m.Signature == "" || (len(m.Signature) < 8) {
		return fmt.Errorf("missing or short signature")
	}
	for _, rule := range m.Permissions.Network {
		if !networkRE.MatchString(rule) {
			return fmt.Errorf("invalid network permission %q: must be 'METHOD URL_PATTERN'", rule)
		}
	}
	switch m.Permissions.Filesystem {
	case "", "none", "read-only", "read-write":
	default:
		return fmt.Errorf("invalid filesystem permission: %q", m.Permissions.Filesystem)
	}
	switch m.Permissions.Shell {
	case "", "none", "allow":
	default:
		return fmt.Errorf("invalid shell permission: %q", m.Permissions.Shell)
	}
	switch m.Permissions.Memory {
	case "", "none", "own", "shared":
	default:
		return fmt.Errorf("invalid memory permission: %q", m.Permissions.Memory)
	}
	switch m.Language {
	case "", "node", "python":
	default:
		return fmt.Errorf("invalid language %q (want \"node\" or \"python\")", m.Language)
	}
	return nil
}
