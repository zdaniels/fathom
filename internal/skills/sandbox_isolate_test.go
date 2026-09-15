package skills

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewSandboxConfig(t *testing.T) {
	// Empty paths produce a disabled config (so non-isolating callers get a
	// plain launch).
	if c := newSandboxConfig(); c.enabled {
		t.Error("config with no paths should be disabled")
	}
	if c := newSandboxConfig("", ""); c.enabled {
		t.Error("config with only empty paths should be disabled")
	}
	c := newSandboxConfig("/tmp/x/vault", "/tmp/x/.master.key")
	if !c.enabled || len(c.protect) != 2 {
		t.Fatalf("expected enabled config with 2 paths, got enabled=%v n=%d", c.enabled, len(c.protect))
	}
	for _, p := range c.protect {
		if !filepath.IsAbs(p) {
			t.Errorf("protected path %q should be absolute", p)
		}
	}
}

func TestMacProfileShape(t *testing.T) {
	c := newSandboxConfig("/Users/x/.fantazm/vault", "/Users/x/.fantazm/.master.key")
	prof := c.macProfile()
	for _, want := range []string{
		"(version 1)",
		"(allow default)",
		"(deny file-read*",
		"(deny file-write*",
		`/usr/bin/security`,
		`.master.key`,
		`vault`,
	} {
		if !strings.Contains(prof, want) {
			t.Errorf("profile missing %q\nprofile: %s", want, prof)
		}
	}
}

func TestSBPLStringEscapes(t *testing.T) {
	got := sbplString(`/a/b"c\d`)
	if got != `"/a/b\"c\\d"` {
		t.Errorf("sbplString escaping wrong: %s", got)
	}
}

// TestSandboxDeniesProtectedFile is the end-to-end proof of the H2 fix: a
// process launched through the isolation wrapper must not be able to read a
// file we marked protected. Skipped where the platform tools aren't present.
func TestSandboxDeniesProtectedFile(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("OS sandbox proof currently exercised on darwin (sandbox-exec)")
	}
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not available")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	dir := t.TempDir()
	secret := filepath.Join(dir, "vault")
	if err := os.WriteFile(secret, []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join(dir, "ok.txt")
	if err := os.WriteFile(allowed, []byte("FINE"), 0o600); err != nil {
		t.Fatal(err)
	}

	script := `const fs=require("fs");try{process.stdout.write("READ:"+fs.readFileSync(process.argv[1],"utf8"))}catch(e){process.stdout.write("DENIED:"+e.code)}`

	cfg := newSandboxConfig(secret)

	run := func(target string) string {
		cmd := cfg.wrap(context.Background(), node, []string{"-e", script, target})
		out, _ := cmd.CombinedOutput()
		return string(out)
	}

	if got := run(secret); !strings.Contains(got, "DENIED") {
		t.Errorf("reading protected file should be denied, got: %q", got)
	}
	if got := run(allowed); !strings.Contains(got, "READ:FINE") {
		t.Errorf("reading an unprotected file should succeed, got: %q", got)
	}
}
