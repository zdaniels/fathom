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

// CI opts in on a Docker-enabled runner. This probes the actual isolation
// boundary, not just command-line flags, without real credentials or services.
func TestRequiredSandboxContainerBoundary(t *testing.T) {
	if os.Getenv("FATHOM_TEST_SKILL_CONTAINER") != "1" {
		t.Skip("requires Docker-enabled integration runner")
	}
	t.Setenv("FATHOM_SKILL_SANDBOX", "required")
	dir := t.TempDir()
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "secret")
	if err := os.WriteFile(secret, []byte("must-not-be-readable"), 0600); err != nil {
		t.Fatal(err)
	}
	runner, err := os.ReadFile("runner.js")
	if err != nil {
		t.Fatal(err)
	}
	runnerPath := filepath.Join(dir, "runner.js")
	os.WriteFile(runnerPath, runner, 0600)
	skillDir := filepath.Join(dir, "skill")
	os.Mkdir(skillDir, 0700)
	code := `import fs from 'node:fs';
 export async function run(input) {
 let readable=false,writable=false,network=false;
 try {fs.readFileSync(input.secret);readable=true;}catch{}
 try {fs.writeFileSync('/skill/write','no');writable=true;}catch{}
 try {await fetch('http://1.1.1.1',{signal:AbortSignal.timeout(1500)});network=true;}catch{}
 return {readable,writable,network,uid:process.getuid()};
 }`
	entry := filepath.Join(skillDir, "probe.mjs")
	os.WriteFile(entry, []byte(code), 0600)
	s := NewSandbox(runnerPath, 30000).WithIsolation(secret)
	out, err := s.Invoke(context.Background(), SandboxInvocation{EntryPoint: entry, FunctionName: "run", Input: map[string]string{"secret": secret}})
	if err != nil {
		t.Fatal(err)
	}
	result := out.(map[string]interface{})
	for _, key := range []string{"readable", "writable", "network"} {
		if result[key] != false {
			t.Fatalf("isolation failed: %s = %v", key, result[key])
		}
	}
	if result["uid"] == float64(0) {
		t.Fatal("skill ran as root")
	}
}
