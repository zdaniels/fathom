package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRequiredSandboxFailsWithoutDocker(t *testing.T) {
	t.Setenv("FATHOM_SKILL_SANDBOX", "required")
	t.Setenv("PATH", t.TempDir())
	s := NewSandbox("unused", 1000)
	if _, err := s.Invoke(context.Background(), SandboxInvocation{EntryPoint: "unused", FunctionName: "run"}); err == nil || !strings.Contains(err.Error(), "Docker") {
		t.Fatalf("expected isolation failure, got %v", err)
	}
}
func TestRequiredSandboxNeverFallsBackAfterRuntimeFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FATHOM_SKILL_SANDBOX", "required")
	t.Setenv("PATH", dir)
	for name, content := range map[string]string{"docker": "#!/bin/sh\necho '{\"ok\":true,\"result\":\"unsafe\"}'\nexit 125\n", "skill.js": "export function run(){}", "runner.js": ""} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	s := NewSandbox(filepath.Join(dir, "runner.js"), 1000)
	if _, err := s.Invoke(context.Background(), SandboxInvocation{EntryPoint: filepath.Join(dir, "skill.js"), FunctionName: "run"}); err == nil {
		t.Fatal("failed container returned success")
	}
}
func TestRequiredSandboxMountsNoHostRoot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"), 0700)
	os.WriteFile(filepath.Join(dir, "skill.js"), nil, 0600)
	s := NewSandbox(filepath.Join(dir, "runner.js"), 1000)
	inv := SandboxInvocation{EntryPoint: filepath.Join(dir, "skill.js")}
	cmd, err := s.isolatedCommand(context.Background(), &inv, []string{"--no-warnings", s.runnerPath})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Args, " ")
	for _, flag := range []string{"--network=none", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "dst=/skill,readonly", "dst=/runner.js,readonly"} {
		if !strings.Contains(joined, flag) {
			t.Fatal("missing " + flag)
		}
	}
	if strings.Contains(joined, "src=/,") {
		t.Fatal("mounted host root")
	}
	if inv.EntryPoint != "/skill/skill.js" {
		t.Fatal(inv.EntryPoint)
	}
}
