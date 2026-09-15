package agentfactory

import (
	"os"
	"testing"
)

func TestResumeRecordsPersistAndSeparateIdentities(t *testing.T) {
	dir := t.TempDir()
	path := resumePath(dir, "claude", "model", "workspace", "user", "thread")
	if err := saveResume(path, "provider-session"); err != nil {
		t.Fatal(err)
	}
	session, err := loadResume(resumePath(dir, "claude", "model", "workspace", "user", "thread"))
	if err != nil || session != "provider-session" {
		t.Fatalf("%q %v", session, err)
	}
	for _, identity := range [][]string{{"claude", "model", "workspace", "other", "thread"}, {"claude", "model", "other", "user", "thread"}, {"codex", "model", "workspace", "user", "thread"}} {
		if got, err := loadResume(resumePath(dir, identity...)); err != nil || got != "" {
			t.Fatal("identity collision")
		}
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("resume record not private")
	}
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadResume(path); err == nil {
		t.Fatal("corruption silently discarded")
	}
}
