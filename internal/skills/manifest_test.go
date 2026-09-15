package skills

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zdaniels/fathom/pkg/types"
)

const validManifest = `name: testskill
version: "1.2.3"
author: tester
signature: ed25519:builtin
description: Test skill
permissions:
  network:
    - GET https://api.example.com/*
  filesystem: none
  shell: none
  memory: own
  secrets:
    - TEST_TOKEN
tools:
  - name: do_a
    function: doA
    description: Do A
    parameters:
      type: object
      properties: {}
      required: []
`

func TestParseManifestAcceptsValid(t *testing.T) {
	m, err := ParseManifest([]byte(validManifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Name != "testskill" {
		t.Errorf("Name = %q, want testskill", m.Name)
	}
	if len(m.Tools) != 1 || m.Tools[0].Function != "doA" {
		t.Errorf("tools = %+v", m.Tools)
	}
}

func TestParseManifestRejectsBadName(t *testing.T) {
	bad := []string{"Bad-Name", "starts-with-Cap", "has space", "ends-with-dash-"}
	for _, n := range bad {
		m := types.SkillManifest{Name: n, Version: "1.0.0", Author: "a", Signature: "ed25519:b"}
		if err := ValidateManifest(m); err == nil {
			t.Errorf("ValidateManifest accepted bad name %q", n)
		}
	}
}

func TestParseManifestRejectsBadVersion(t *testing.T) {
	m := types.SkillManifest{Name: "ok", Version: "v1", Author: "a", Signature: "ed25519:b"}
	if err := ValidateManifest(m); err == nil {
		t.Error("ValidateManifest accepted non-semver version")
	}
}

func TestParseManifestRejectsBadNetworkRule(t *testing.T) {
	m := types.SkillManifest{
		Name: "ok", Version: "1.0.0", Author: "a", Signature: "ed25519:b",
		Permissions: types.SkillPermissions{
			Network: []string{"https://example.com"},
		},
	}
	if err := ValidateManifest(m); err == nil {
		t.Error("ValidateManifest accepted network rule without METHOD prefix")
	}
}

func TestLoadManifestFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "skill.manifest.yaml")
	os.WriteFile(path, []byte(validManifest), 0o644)
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.Name != "testskill" {
		t.Errorf("Name = %q", m.Name)
	}
}
