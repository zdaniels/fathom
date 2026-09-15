package agentfactory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zdaniels/fathom/internal/security"
)

// Each private record is replaced atomically. Keeping the provider, workspace,
// user and conversation in the key prevents accidentally resuming another
// provider's or workspace's session after a configuration change.
func resumePath(dir string, identity ...string) string {
	if dir == "" {
		return ""
	}
	b, _ := json.Marshal(identity)
	return filepath.Join(dir, "takeover-sessions", security.SHA256Hex(string(b))+".json")
}
func loadResume(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var record struct {
		Session string `json:"session"`
	}
	if err = json.Unmarshal(b, &record); err != nil {
		return "", fmt.Errorf("read takeover session: %w", err)
	}
	return record.Session, nil
}
func saveResume(path, session string) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".resume-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	b, _ := json.Marshal(map[string]string{"session": session})
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
