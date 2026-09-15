package collab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zdaniels/fathom/internal/agent/llm"
	"github.com/zdaniels/fathom/internal/security"
	"github.com/zdaniels/fathom/pkg/types"
)

func TestWorkspaceContainers(t *testing.T) {
	image := os.Getenv("FATHOM_TEST_WORKSPACE_IMAGE")
	if image == "" {
		t.Skip("set FATHOM_TEST_WORKSPACE_IMAGE to a locally built Fathom image")
	}
	t.Setenv("FATHOM_TEST_SECRET", "must-not-reach-worker")
	var script string
	toolName := "shell"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		last := in.Messages[len(in.Messages)-1]
		if last.Role == "tool" {
			if !strings.Contains(last.Content, "BOUNDARY_OK") {
				t.Errorf("boundary check failed: %s", last.Content)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": "Verified handoff"}, "finish_reason": "stop"}}})
			return
		}
		args, _ := json.Marshal(map[string]string{"command": script})
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"tool_calls": []any{map[string]any{"id": "c1", "function": map[string]string{"name": toolName, "arguments": string(args)}}}}, "finish_reason": "tool_calls"}}})
	}))
	defer server.Close()
	router, err := llm.NewRouter(types.LLMConfig{Provider: "openai", Model: "test", BaseURL: server.URL}, func(string) (string, error) { return "coordinator-only", nil })
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Router: router, Image: image}
	a, b := security.GenerateID(), security.GenerateID()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = command(ctx, "", "volume", "rm", volume(a), volume(b))
	}()
	base := `set -eu
test "$(id -u)" = 65532
test ! -e /var/run/docker.sock
test ! -e /Users
test -z "${FATHOM_TEST_SECRET:-}"
if touch /etc/forbidden 2>/dev/null; then exit 1; fi
node -e 'const s=require("net").connect(443,"1.1.1.1");s.on("connect",()=>process.exit(1));s.on("error",()=>process.exit(0));setTimeout(()=>process.exit(1),2000)'
`
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	script = base + "echo alpha > /workspace/alpha\necho BOUNDARY_OK\n"
	var progress []CommandRecord
	built, err := runner.runWithProgress(ctx, Task{WorkspaceID: a, Title: "Build"}, false, func(c CommandRecord) { progress = append(progress, c) })
	if err != nil {
		t.Fatal(err)
	}
	if len(progress) != 1 || progress[0].ExitCode != 0 {
		t.Fatalf("missing command progress: %+v", progress)
	}
	if built.Evidence.Warning != "" || len(built.Evidence.Changes) != 1 || built.Evidence.Changes[0].After != "alpha\n" || built.Evidence.Commands[0].ExitCode != 0 {
		t.Fatalf("bad build evidence: %+v", built.Evidence)
	}
	script = base + "test -f /workspace/alpha\nif touch /workspace/reviewer-write 2>/dev/null; then exit 1; fi\necho BOUNDARY_OK\n"
	toolName = "test"
	script += "exit 7\n"
	reviewed, err := runner.RunWithEvidence(ctx, Task{WorkspaceID: a, Title: "Review", Handoff: "Inspect alpha"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(reviewed.Evidence.Changes) != 0 || len(reviewed.Evidence.Commands) != 1 || reviewed.Evidence.Commands[0].Kind != "test" || reviewed.Evidence.Commands[0].ExitCode != 7 {
		t.Fatalf("failed test was not recorded: %+v", reviewed.Evidence)
	}
	toolName = "shell"
	script = "echo beta > /workspace/alpha\necho added > /workspace/new.txt\necho BOUNDARY_OK\n"
	updated, err := runner.RunWithEvidence(ctx, Task{WorkspaceID: a, Title: "Update"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Evidence.Changes) != 2 || updated.Evidence.Changes[0].Kind != "modified" || updated.Evidence.Changes[0].Before != "alpha\n" || updated.Evidence.Changes[0].After != "beta\n" {
		t.Fatalf("bad update evidence: %+v", updated.Evidence)
	}
	script = base + "test ! -e /workspace/alpha\necho BOUNDARY_OK\n"
	if _, err = runner.Run(ctx, Task{WorkspaceID: b, Title: "Other workspace"}, false); err != nil {
		t.Fatal(err)
	}
}
