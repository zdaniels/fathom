package builtin

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/zdaniels/fathom/internal/agent"
)

// dockerAvailable returns true if `docker` is on PATH AND the daemon
// responds to `docker info`. Most CI runners won't have it; we skip
// those tests rather than fail.
func dockerAvailable() bool {
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	return exec.Command("docker", "info").Run() == nil
}

func TestPythonExecRunsCode(t *testing.T) {
	if !dockerAvailable() {
		t.Skip("docker not available")
	}
	out, err := PythonExecTool().Execute(context.Background(),
		map[string]interface{}{"code": "print('hello', 1+2)"},
		agent.ToolContext{Policy: allowAllPolicy()})
	if err != nil {
		t.Fatalf("python_exec: %v", err)
	}
	m := out.(map[string]interface{})
	if !strings.Contains(m["stdout"].(string), "hello 3") {
		t.Errorf("stdout = %q, want 'hello 3'", m["stdout"])
	}
}

func TestPythonExecCapturesNonZero(t *testing.T) {
	if !dockerAvailable() {
		t.Skip("docker not available")
	}
	out, _ := PythonExecTool().Execute(context.Background(),
		map[string]interface{}{"code": "import sys; sys.exit(7)"},
		agent.ToolContext{Policy: allowAllPolicy()})
	if out.(map[string]interface{})["exitCode"].(int) != 7 {
		t.Errorf("exitCode = %v, want 7", out)
	}
}

func TestPythonExecBlockedByPolicy(t *testing.T) {
	if !dockerAvailable() {
		t.Skip("docker not available")
	}
	_, err := PythonExecTool().Execute(context.Background(),
		map[string]interface{}{"code": "print('blocked')"},
		agent.ToolContext{Policy: denyShellPolicy()})
	if err == nil || !strings.Contains(err.Error(), "blocked by policy") {
		t.Errorf("expected policy denial, got %v", err)
	}
}

func TestPythonExecMissingCode(t *testing.T) {
	_, err := PythonExecTool().Execute(context.Background(),
		map[string]interface{}{"code": ""},
		agent.ToolContext{Policy: allowAllPolicy()})
	if err == nil || !strings.Contains(err.Error(), "code is required") {
		t.Errorf("expected 'code is required' error, got %v", err)
	}
}

func TestPythonExecReportsDockerMissing(t *testing.T) {
	if _, err := exec.LookPath("docker"); err == nil && exec.Command("docker", "info").Run() == nil {
		t.Skip("docker IS available — this test only meaningful when it's missing")
	}
	// When docker isn't around, executing should return the helpful error
	// rather than a generic exec error.
	_, err := PythonExecTool().Execute(context.Background(),
		map[string]interface{}{"code": "print('x')"},
		agent.ToolContext{Policy: allowAllPolicy()})
	if err == nil {
		t.Fatal("expected error when docker is missing")
	}
	// We just verify it's an error, not the exact text, since the OS varies.
	var pathErr *exec.Error
	if !errors.As(err, &pathErr) && !strings.Contains(err.Error(), "Docker") && !strings.Contains(err.Error(), "docker") {
		t.Errorf("error should mention docker, got %v", err)
	}
}

func TestImageGenerateRequiresPrompt(t *testing.T) {
	_, err := ImageGenerateTool().Execute(context.Background(),
		map[string]interface{}{},
		agent.ToolContext{})
	if err == nil || !strings.Contains(err.Error(), "prompt is required") {
		t.Errorf("expected 'prompt is required', got %v", err)
	}
}

func TestImageGenerateRequiresAPIKey(t *testing.T) {
	_, err := ImageGenerateTool().Execute(context.Background(),
		map[string]interface{}{"prompt": "a cat"},
		agent.ToolContext{
			GetSecret: func(string) (string, error) { return "", errors.New("missing") },
		})
	if err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Errorf("expected OPENAI_API_KEY error, got %v", err)
	}
}
