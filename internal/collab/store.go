// Package collab implements shared workspaces independently of private chats.
package collab

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zdaniels/fathom/internal/security"
	_ "modernc.org/sqlite"
)

var ErrConflict = errors.New("task changed; refresh before trying again")
var ErrForbidden = errors.New("workspace membership does not permit this action")

type Workspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role,omitempty"`
}
type Member struct {
	UserID string `json:"userId"`
	Role   string `json:"role"`
}
type Task struct {
	BuildRevision  int       `json:"buildRevision,omitempty"`
	ReviewRevision int       `json:"reviewRevision,omitempty"`
	ID             string    `json:"id"`
	WorkspaceID    string    `json:"workspaceId"`
	Title          string    `json:"title"`
	Description    string    `json:"description"`
	State          string    `json:"state"`
	Assignee       string    `json:"assignee"`
	Builder        string    `json:"builder"`
	Reviewer       string    `json:"reviewer"`
	Handoff        string    `json:"handoff"`
	Review         string    `json:"review"`
	Source         string    `json:"source,omitempty"`
	ExternalID     string    `json:"externalId,omitempty"`
	ExternalURL    string    `json:"externalUrl,omitempty"`
	Revision       int       `json:"revision"`
	UpdatedAt      time.Time `json:"updatedAt"`
}
type Activity struct {
	ID     int64     `json:"id"`
	TaskID string    `json:"taskId"`
	Actor  string    `json:"actor"`
	Kind   string    `json:"kind"`
	Text   string    `json:"text"`
	At     time.Time `json:"at"`
}
type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;
 CREATE TABLE IF NOT EXISTS workspaces(id TEXT PRIMARY KEY,name TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS members(workspace_id TEXT REFERENCES workspaces(id) ON DELETE CASCADE,user_id TEXT,role TEXT NOT NULL,PRIMARY KEY(workspace_id,user_id));
 CREATE TABLE IF NOT EXISTS tasks(id TEXT PRIMARY KEY,workspace_id TEXT REFERENCES workspaces(id) ON DELETE CASCADE,state TEXT NOT NULL,revision INTEGER NOT NULL,record BLOB NOT NULL);
 CREATE UNIQUE INDEX IF NOT EXISTS one_workspace_run ON tasks(workspace_id) WHERE state='running';
 CREATE TABLE IF NOT EXISTS activity(id INTEGER PRIMARY KEY AUTOINCREMENT,workspace_id TEXT REFERENCES workspaces(id) ON DELETE CASCADE,task_id TEXT NOT NULL,record BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS run_evidence(workspace_id TEXT REFERENCES workspaces(id) ON DELETE CASCADE,task_id TEXT REFERENCES tasks(id) ON DELETE CASCADE,role TEXT NOT NULL,record BLOB NOT NULL,PRIMARY KEY(workspace_id,task_id,role));
 CREATE TABLE IF NOT EXISTS task_connections(workspace_id TEXT REFERENCES workspaces(id) ON DELETE CASCADE,provider TEXT,scope TEXT,site TEXT,email TEXT,token_secret TEXT,revision INTEGER NOT NULL,enabled INTEGER NOT NULL,PRIMARY KEY(workspace_id,provider));
 CREATE INDEX IF NOT EXISTS workspace_activity ON activity(workspace_id,id);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Role(workspace, user string) string {
	var role string
	_ = s.db.QueryRow("SELECT role FROM members WHERE workspace_id=? AND user_id=?", workspace, user).Scan(&role)
	return role
}
func (s *Store) CreateWorkspace(name, user string) (Workspace, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return Workspace{}, errors.New("workspace name must be 1–100 characters")
	}
	w := Workspace{ID: security.GenerateID(), Name: name, Role: "admin"}
	tx, err := s.db.Begin()
	if err != nil {
		return w, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO workspaces VALUES(?,?)", w.ID, w.Name); err != nil {
		return w, err
	}
	if _, err = tx.Exec("INSERT INTO members VALUES(?,?,?)", w.ID, user, "admin"); err != nil {
		return w, err
	}
	return w, tx.Commit()
}
func (s *Store) Workspaces(user string) ([]Workspace, error) {
	rows, err := s.db.Query("SELECT w.id,w.name,m.role FROM workspaces w JOIN members m ON m.workspace_id=w.id WHERE m.user_id=? ORDER BY w.name", user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Workspace{}
	for rows.Next() {
		var w Workspace
		if err = rows.Scan(&w.ID, &w.Name, &w.Role); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
func (s *Store) Members(workspace string) ([]Member, error) {
	rows, err := s.db.Query("SELECT user_id,role FROM members WHERE workspace_id=? ORDER BY user_id", workspace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		if err = rows.Scan(&m.UserID, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func (s *Store) SetMember(workspace, actor, user, role string) error {
	if len(user) == 0 || len(user) > 512 {
		return errors.New("invalid user ID")
	}
	if role != "admin" && role != "member" && role != "viewer" && role != "" {
		return errors.New("invalid workspace role")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Acquire the SQLite writer lock before checking the last-admin invariant.
	if _, err = tx.Exec("UPDATE workspaces SET name=name WHERE id=?", workspace); err != nil {
		return err
	}
	var actorRole string
	if err = tx.QueryRow("SELECT role FROM members WHERE workspace_id=? AND user_id=?", workspace, actor).Scan(&actorRole); err != nil || actorRole != "admin" {
		return ErrForbidden
	}
	var old string
	_ = tx.QueryRow("SELECT role FROM members WHERE workspace_id=? AND user_id=?", workspace, user).Scan(&old)
	if old == "admin" && role != "admin" {
		var n int
		if err = tx.QueryRow("SELECT count(*) FROM members WHERE workspace_id=? AND role='admin'", workspace).Scan(&n); err != nil {
			return err
		}
		if n <= 1 {
			return errors.New("workspace must retain an administrator")
		}
	}
	if role == "" {
		_, err = tx.Exec("DELETE FROM members WHERE workspace_id=? AND user_id=?", workspace, user)
	} else {
		_, err = tx.Exec("INSERT INTO members VALUES(?,?,?) ON CONFLICT(workspace_id,user_id) DO UPDATE SET role=excluded.role", workspace, user, role)
	}
	if err != nil {
		return err
	}
	if err = activity(tx, workspace, "", actor, "membership", user+": "+role); err != nil {
		return err
	}
	return tx.Commit()
}
func validTask(t Task) error {
	if strings.TrimSpace(t.Title) == "" || len(t.Title) > 250 || len(t.Description) > 32000 || len(t.Handoff) > 128000 || len(t.Review) > 128000 || len(t.Assignee) > 512 || len(t.Builder) > 100 || len(t.Reviewer) > 100 {
		return errors.New("invalid task fields or size")
	}
	switch t.State {
	case "backlog", "ready", "running", "review", "blocked", "done":
		return nil
	}
	return errors.New("invalid task state")
}
func activity(tx *sql.Tx, workspace, task, actor, kind, text string) error {
	b, err := json.Marshal(Activity{TaskID: task, Actor: actor, Kind: kind, Text: text, At: time.Now().UTC()})
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO activity(workspace_id,task_id,record) VALUES(?,?,?)", workspace, task, b)
	return err
}
func (s *Store) CreateTask(t Task, actor string) (Task, error) {
	t.ID = security.GenerateID()
	t.State = "backlog"
	t.Revision = 1
	t.UpdatedAt = time.Now().UTC()
	if err := validTask(t); err != nil {
		return t, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return t, err
	}
	defer tx.Rollback()
	b, _ := json.Marshal(t)
	if _, err = tx.Exec("INSERT INTO tasks VALUES(?,?,?,?,?)", t.ID, t.WorkspaceID, t.State, t.Revision, b); err != nil {
		return t, err
	}
	if err = activity(tx, t.WorkspaceID, t.ID, actor, "created", t.Title); err != nil {
		return t, err
	}
	return t, tx.Commit()
}
func (s *Store) Task(workspace, id string) (Task, error) {
	var t Task
	var b []byte
	err := s.db.QueryRow("SELECT record FROM tasks WHERE workspace_id=? AND id=?", workspace, id).Scan(&b)
	if err == nil {
		err = json.Unmarshal(b, &t)
	}
	return t, err
}
func (s *Store) Tasks(workspace string) ([]Task, error) {
	rows, err := s.db.Query("SELECT record FROM tasks WHERE workspace_id=? ORDER BY rowid DESC LIMIT 1000", workspace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		var b []byte
		var t Task
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Save uses compare-and-swap to prevent duplicate starts and lost human edits.
func (s *Store) Save(t Task, actor, kind, text string) (Task, error) {
	if err := validTask(t); err != nil {
		return t, err
	}
	if len(text) > 128000 {
		return t, errors.New("activity too long")
	}
	old := t.Revision
	t.Revision++
	t.UpdatedAt = time.Now().UTC()
	b, _ := json.Marshal(t)
	tx, err := s.db.Begin()
	if err != nil {
		return t, err
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE tasks SET state=?,revision=?,record=? WHERE workspace_id=? AND id=? AND revision=?", t.State, t.Revision, b, t.WorkspaceID, t.ID, old)
	if err != nil {
		return t, fmt.Errorf("save task (only one run per workspace is allowed): %w", err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return t, ErrConflict
	}
	if err = activity(tx, t.WorkspaceID, t.ID, actor, kind, text); err != nil {
		return t, err
	}
	return t, tx.Commit()
}
func (s *Store) Activity(workspace string) ([]Activity, error) {
	rows, err := s.db.Query("SELECT id,record FROM activity WHERE workspace_id=? ORDER BY id DESC LIMIT 200", workspace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Activity{}
	for rows.Next() {
		var id int64
		var b []byte
		var a Activity
		if err = rows.Scan(&id, &b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &a); err != nil {
			return nil, err
		}
		a.ID = id
		out = append(out, a)
	}
	return out, rows.Err()
}

// Recover is called before accepting requests. Interrupted work requires an
// explicit retry; restarting the gateway never silently repeats tool actions.
func (s *Store) Recover() error {
	rows, err := s.db.Query("SELECT record FROM tasks WHERE state='running'")
	if err != nil {
		return err
	}
	var tasks []Task
	for rows.Next() {
		var b []byte
		var t Task
		if err = rows.Scan(&b); err != nil {
			break
		}
		if err = json.Unmarshal(b, &t); err != nil {
			break
		}
		tasks = append(tasks, t)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, t := range tasks {
		t.State = "blocked"
		if _, err = s.Save(t, "system", "interrupted", "Gateway restarted during this run. Inspect workspace files before retrying."); err != nil {
			return err
		}
	}
	return nil
}

// AddComment appends discussion without revising the task. A running agent can
// therefore save its outcome using the revision it started with. Recheck the
// membership in the same writer transaction as the append.
func (s *Store) AddComment(workspace, task, user, text string) error {
	if strings.TrimSpace(text) == "" || len(text) > 64000 {
		return errors.New("comment must be 1–64000 bytes")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec("UPDATE tasks SET revision=revision WHERE workspace_id=? AND id=?", workspace, task)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return errors.New("task not found")
	}
	var role string
	if err = tx.QueryRow("SELECT role FROM members WHERE workspace_id=? AND user_id=?", workspace, user).Scan(&role); err != nil || (role != "member" && role != "admin") {
		return ErrForbidden
	}
	if err = activity(tx, workspace, task, user, "comment", text); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) AddProgress(workspace, task, actor, text string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = activity(tx, workspace, task, actor, "command_completed", text); err != nil {
		return err
	}
	return tx.Commit()
}
