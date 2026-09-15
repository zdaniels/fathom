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

	"github.com/zdaniels/fathom/internal/security"
)

func TestDemoPermissionAndAtomicTasks(t *testing.T) {
	store := newStore(t)
	s := &Service{Store: store, Auth: func(*http.Request) (string, error) { return "alice", nil }, CanCreateWorkspace: func(string) bool { return false }}
	request := func() int {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/board/demo", strings.NewReader("{}")))
		return w.Code
	}
	if got := request(); got != 403 {
		t.Fatal(got)
	}
	s.CanCreateWorkspace = func(string) bool { return true }
	if got := request(); got != 503 {
		t.Fatal(got)
	}
	ws, _ := store.Workspaces("alice")
	if len(ws) != 0 {
		t.Fatal("failed seed left a workspace")
	}
	w := Workspace{ID: "demo", Name: "Demo", Role: "admin"}
	tasks := demoTasks()
	tasks[1].State = "invalid"
	if err := store.createProject(w, "alice", tasks); err == nil {
		t.Fatal("invalid task accepted")
	}
	ws, _ = store.Workspaces("alice")
	if len(ws) != 0 {
		t.Fatal("partial project committed")
	}
	if err := store.createProject(w, "alice", demoTasks()); err != nil {
		t.Fatal(err)
	}
	saved, _ := store.Tasks(w.ID)
	if len(saved) != 2 {
		t.Fatal(saved)
	}
	for _, task := range saved {
		if task.State == "running" || task.Handoff != "" || task.Review != "" {
			t.Fatal("demo fabricated an agent run")
		}
	}
}
func TestWorkspaceContainersDemo(t *testing.T) {
	image := os.Getenv("FATHOM_TEST_WORKSPACE_IMAGE")
	if image == "" {
		t.Skip("set FATHOM_TEST_WORKSPACE_IMAGE")
	}
	store := newStore(t)
	runner := &Runner{Image: image}
	s := &Service{Store: store, Runner: runner, Auth: func(*http.Request) (string, error) { return "alice", nil }, CanCreateWorkspace: func(string) bool { return true }}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/board/demo", strings.NewReader("{}")))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var ws Workspace
	if err := json.Unmarshal(w.Body.Bytes(), &ws); err != nil {
		t.Fatal(err)
	}
	defer removeProjectVolume(ws.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := runner.seedProject(ctx, ws.ID, demoProject()); err == nil {
		t.Fatal("overwrote existing workspace")
	}
	// A separate disposable container verifies that the actual demo tests fail,
	// then pass after the intended fix. No provider credentials are needed.
	name := "fathom-demo-test-" + security.GenerateID()
	defer command(context.Background(), "", "rm", "-f", name)
	out, err := command(ctx, "", "run", "--rm", "--name", name, "--pull=never", "--network=none", "--read-only", "--user=65532:65532", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pids-limit=32", "--memory=128m", "--cpus=1", "--mount", "type=volume,src="+volume(ws.ID)+",dst=/workspace", "--workdir=/workspace", "--entrypoint=/bin/sh", image, "-c", `set -eu; if node --test shipping.test.js; then exit 1; fi; printf 'module.exports = total => total < 50 ? total + 7 : total;\n' > shipping.js; node --test shipping.test.js`)
	if err != nil {
		t.Fatal(err, out)
	}
}
