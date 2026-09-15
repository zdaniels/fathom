package collab

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/zdaniels/fathom/internal/security"
)

type projectFile struct {
	Name string
	Body []byte
	Mode int64
}

func demoProject() []projectFile {
	return []projectFile{
		{"README.md", []byte("# Shipping calculator demo\n\nRun `node --test shipping.test.js`. The calculator has an intentional bug: orders of $50 or more should ship free. Fix the implementation without weakening tests, run a reviewer, then approve it on the board. No packages or network access needed.\n"), 0644},
		{"shipping.js", []byte("module.exports = total => total + 7;\n"), 0644},
		{"shipping.test.js", []byte("const {test} = require('node:test');\nconst assert = require('node:assert/strict');\nconst shipping = require('./shipping');\ntest('shipping below threshold', () => assert.equal(shipping(25), 32));\ntest('free shipping at threshold', () => assert.equal(shipping(50), 50));\ntest('free shipping above threshold', () => assert.equal(shipping(75), 75));\n"), 0644},
	}
}
func demoTasks() []Task {
	return []Task{
		{Title: "Fix free shipping at $50", State: "ready", Description: "Inspect README.md, shipping.js and shipping.test.js. Run node --test shipping.test.js with the test tool to observe the failing cases. Fix shipping.js so orders below $50 cost $7 extra, and orders of $50 or more ship free. Preserve the tests, run them again with the test tool, and give a handoff. After the builder finishes, a human should run the independent reviewer and approve completion."},
		{Title: "Add a zero-dollar order regression test", State: "backlog", Description: "After the shipping bug is fixed and reviewed, add a test that a $0 order totals $7. Keep existing tests and run node --test shipping.test.js with the test tool. Report the results for independent review."},
	}
}
func projectTar(files []projectFile) ([]byte, error) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.Name, Mode: f.Mode, Size: int64(len(f.Body)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.Body); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Seed only a fresh, unpublished workspace. No existing files are replaced.
func (r *Runner) seedProject(ctx context.Context, workspace string, files []projectFile) error {
	if r == nil || r.Image == "" {
		return errors.New("configure collaboration.image and start Docker to create a project")
	}
	data, err := projectTar(files)
	if err != nil {
		return err
	}
	name := "fathom-seed-" + security.GenerateID()
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = command(c, "", "rm", "-f", name)
	}()
	_, err = command(ctx, string(data), "run", "--rm", "-i", "--name", name, "--pull=never", "--network=none", "--read-only", "--user=65532:65532", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pids-limit=32", "--memory=128m", "--cpus=1", "--mount", "type=volume,src="+volume(workspace)+",dst=/workspace", "--entrypoint=/bin/sh", r.Image, "-c", `test -z "$(ls -A /workspace)" && tar -xf - -C /workspace --no-same-owner`)
	if err != nil {
		return errors.New("project files could not be initialized; check Docker and collaboration.image")
	}
	return nil
}
func removeProjectVolume(workspace string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = command(ctx, "", "volume", "rm", volume(workspace))
}
func (s *Store) createProject(w Workspace, user string, tasks []Task) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO workspaces VALUES(?,?)", w.ID, w.Name); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO members VALUES(?,?,?)", w.ID, user, "admin"); err != nil {
		return err
	}
	for _, t := range tasks {
		t.ID = security.GenerateID()
		t.WorkspaceID = w.ID
		t.Revision = 1
		t.UpdatedAt = time.Now().UTC()
		if err = validTask(t); err != nil {
			return err
		}
		b, _ := json.Marshal(t)
		if _, err = tx.Exec("INSERT INTO tasks VALUES(?,?,?,?,?)", t.ID, w.ID, t.State, t.Revision, b); err != nil {
			return err
		}
		if err = activity(tx, w.ID, t.ID, user, "created", t.Title); err != nil {
			return err
		}
	}
	if err = activity(tx, w.ID, "", user, "project_created", w.Name); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Service) createDemo(w http.ResponseWriter, r *http.Request, user string) {
	if r.Method != "POST" {
		fail(w, 405, errors.New("POST required"))
		return
	}
	if s.CanCreateWorkspace == nil || !s.CanCreateWorkspace(user) {
		fail(w, 403, ErrForbidden)
		return
	}
	var in struct{}
	if !decode(w, r, &in) {
		return
	}
	s.finishProject(w, r, user, "Shipping demo", demoProject(), demoTasks())
}
func (s *Service) finishProject(w http.ResponseWriter, r *http.Request, user, name string, files []projectFile, tasks []Task) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		fail(w, 400, errors.New("workspace name must be 1–100 characters"))
		return
	}
	// Bound simultaneous uploads/seeding without blocking comments and agent progress.
	if !s.projectMu.TryLock() {
		fail(w, 409, errors.New("another project is being initialized; try again shortly"))
		return
	}
	defer s.projectMu.Unlock()
	ws := Workspace{ID: security.GenerateID(), Name: name, Role: "admin"}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := s.Runner.seedProject(ctx, ws.ID, files); err != nil {
		if s.Runner != nil && s.Runner.Image != "" {
			removeProjectVolume(ws.ID)
		}
		fail(w, 503, err)
		return
	}
	committed := false
	defer func() {
		if !committed {
			removeProjectVolume(ws.ID)
		}
	}()
	if ctx.Err() != nil {
		fail(w, 503, errors.New("project initialization interrupted"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		fail(w, 503, errors.New("gateway is stopping"))
		return
	}
	if !s.CanCreateWorkspace(user) {
		fail(w, 403, ErrForbidden)
		return
	}
	if err := s.Store.createProject(ws, user, tasks); err != nil {
		fail(w, 500, errors.New("project could not be saved"))
		return
	}
	committed = true
	respond(w, 201, ws)
}
