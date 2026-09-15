package skills

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zdaniels/fathom/pkg/types"
)

// InstalledSkill is one skill discovered on disk + verified.
type InstalledSkill struct {
	Manifest    types.SkillManifest
	Path        string
	Enabled     bool
	InstalledAt time.Time
}

// Runtime is the interface a skill executor must satisfy. The default
// implementation is the in-process subprocess Sandbox; ChasmRuntime is
// an alternate impl that posts invocations to an external Chasm server.
//
// The signature deliberately matches Sandbox.Invoke so the subprocess
// sandbox satisfies it without changes.
type Runtime interface {
	Invoke(ctx context.Context, inv SandboxInvocation) (interface{}, error)
}

// Registry holds installed skills and provides Invoke for execution.
type Registry struct {
	mu      sync.RWMutex
	skills  map[string]InstalledSkill
	sandbox *Sandbox // legacy: kept so existing callers compile
	runtime Runtime  // active runtime (defaults to sandbox)
}

// NewRegistry returns an empty registry wired to the subprocess sandbox.
// Use SetRuntime to swap in an alternate executor (e.g. ChasmRuntime).
func NewRegistry(sandbox *Sandbox) *Registry {
	return &Registry{
		skills:  make(map[string]InstalledSkill),
		sandbox: sandbox,
		runtime: sandbox,
	}
}

// SetRuntime overrides the executor for skill invocations. Callers use
// this to plug in a Chasm-backed runtime instead of the subprocess
// sandbox when fathom.config.yaml has skills.runtime: chasm.
func (r *Registry) SetRuntime(rt Runtime) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rt != nil {
		r.runtime = rt
	}
}

// Install reads + validates the manifest at skillDir/skill.manifest.yaml and
// registers the skill if not already installed.
func (r *Registry) Install(skillDir string) (InstalledSkill, error) {
	manifestPath := filepath.Join(skillDir, "skill.manifest.yaml")
	manifest, err := LoadManifest(manifestPath)
	if err != nil {
		return InstalledSkill{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.skills[manifest.Name]; exists {
		return InstalledSkill{}, fmt.Errorf("skill %q is already installed", manifest.Name)
	}
	skill := InstalledSkill{
		Manifest:    manifest,
		Path:        skillDir,
		Enabled:     true,
		InstalledAt: time.Now().UTC(),
	}
	r.skills[manifest.Name] = skill
	return skill, nil
}

// Get returns a skill by name.
func (r *Registry) Get(name string) (InstalledSkill, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.skills[name]
	return s, ok
}

// List returns every installed skill.
func (r *Registry) List() []InstalledSkill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]InstalledSkill, 0, len(r.skills))
	for _, s := range r.skills {
		out = append(out, s)
	}
	return out
}

// Invocation is what a tool's execute handler ships into Invoke.
type Invocation struct {
	FunctionName string
	Input        interface{}
	Secrets      map[string]string
	Egress       *EgressContext
}

// Invoke executes a function in a skill via the subprocess sandbox.
// Entry-point resolution depends on the manifest's language hint:
//
//   - "" / "node": index.js (compiled) → index.ts (dev). Skills with no
//     language field stay on the legacy Node path so old manifests
//     keep working unchanged.
//   - "python":    index.py.
func (r *Registry) Invoke(ctx context.Context, name string, inv Invocation) (interface{}, error) {
	r.mu.RLock()
	skill, ok := r.skills[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("skill not found: %s", name)
	}
	if !skill.Enabled {
		return nil, fmt.Errorf("skill is disabled: %s", name)
	}

	lang := skill.Manifest.Language
	var entry string
	switch lang {
	case "", "node":
		entry = filepath.Join(skill.Path, "index.js")
		if _, err := os.Stat(entry); err != nil {
			entry = filepath.Join(skill.Path, "index.ts")
			if _, err := os.Stat(entry); err != nil {
				return nil, errors.New("skill entry point not found: " + skill.Path + "/index.{js,ts}")
			}
		}
	case "python":
		entry = filepath.Join(skill.Path, "index.py")
		if _, err := os.Stat(entry); err != nil {
			return nil, errors.New("skill entry point not found: " + skill.Path + "/index.py")
		}
	default:
		return nil, fmt.Errorf("skill %q: unknown language %q in manifest", name, lang)
	}

	return r.runtime.Invoke(ctx, SandboxInvocation{
		EntryPoint:   entry,
		FunctionName: inv.FunctionName,
		Input:        inv.Input,
		Secrets:      inv.Secrets,
		Language:     lang,
		Egress:       inv.Egress,
	})
}
