// Package skills owns the subprocess sandbox + skill registry. The
// language-specific runners are embedded into the Go binary so users
// only need `node` (and/or `python3`) on their PATH — they don't have
// to ship a runner alongside the fathom binary.
package skills

import (
	_ "embed"
	"github.com/zdaniels/fathom/internal/brandenv"
	"os"
	"path/filepath"
)

//go:embed runner.js
var embeddedRunnerJS []byte

//go:embed runner.py
var embeddedRunnerPy []byte

// EnsureRunnerOnDisk extracts the embedded runner.js to ~/.cache/fathom/runner.js
// (overridable via FANTAZM_RUNNER_PATH). Returns the path the sandbox should
// hand to `node`. Re-extracts on every call — cheap and guards against the
// user accidentally editing the cached copy.
func EnsureRunnerOnDisk() (string, error) {
	target := brandenv.Get("FATHOM_RUNNER_PATH")
	if target == "" {
		home, _ := os.UserHomeDir()
		target = filepath.Join(home, ".cache", "fathom", "runner.js")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(target, embeddedRunnerJS, 0o644); err != nil {
		return "", err
	}
	return target, nil
}

// EnsurePythonRunnerOnDisk is the runner.py twin of EnsureRunnerOnDisk.
// Override path via FANTAZM_PY_RUNNER_PATH.
func EnsurePythonRunnerOnDisk() (string, error) {
	target := brandenv.Get("FATHOM_PY_RUNNER_PATH")
	if target == "" {
		home, _ := os.UserHomeDir()
		target = filepath.Join(home, ".cache", "fathom", "runner.py")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(target, embeddedRunnerPy, 0o755); err != nil {
		return "", err
	}
	return target, nil
}
