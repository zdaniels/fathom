package cli

import (
	"bytes"
	"encoding/xml"
	"github.com/zdaniels/fathom/internal/security"
	"github.com/zdaniels/fathom/pkg/types"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServicePinsConfigAndEscapesPaths(t *testing.T) {
	t.Setenv("FATHOM_MODE", "enterprise")
	t.Setenv("FATHOM_WORKSPACE_ROOT", "/tmp/a&b")
	path := filepath.Join(t.TempDir(), "config&test.yaml")
	plist := renderPlist("/tmp/a&b/fathom", "/tmp/a<b", "/tmp/log&x", path)
	decoder := xml.NewDecoder(strings.NewReader(plist))
	var values []string
	for {
		tok, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if start, ok := tok.(xml.StartElement); ok && start.Name.Local == "string" {
			var value string
			if err := decoder.DecodeElement(&value, &start); err != nil {
				t.Fatal(err)
			}
			values = append(values, value)
		}
	}
	joined := strings.Join(values, "|")
	for _, want := range []string{path, "/tmp/a&b/fathom", "enterprise", "/tmp/a&b"} {
		if !strings.Contains(joined, want) {
			t.Fatal("missing " + want)
		}
	}
}
func TestAuditCommandReadsStoredEvents(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfg, []byte("dataDir: ./data\n"), 0600)
	t.Setenv("FATHOM_CONFIG", cfg)
	t.Chdir(t.TempDir())
	audit, err := security.OpenAuditLogger(security.AuditPath(filepath.Join(dir, "data")))
	if err != nil {
		t.Fatal(err)
	}
	audit.Log("session", "alice", types.AuditAuth, nil, types.PolicyAllow)
	audit.Close()
	cmd := newAuditCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "alice") {
		t.Fatal("stored entry missing: " + out.String())
	}
}
