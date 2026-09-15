package collab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Service struct {
	AuditConnection      func(workspace, user, provider, action string)
	CanManageConnections func(*http.Request, string) bool
	ConnectionGuard      func(http.ResponseWriter, *http.Request, string) bool
	SaveSecret           func(string, string) error
	DeleteSecret         func(string)
	ConnectionClient     *http.Client
	connectionMu         sync.Mutex
	projectMu            sync.Mutex
	uploadMu             sync.Mutex
	filesMu              sync.Mutex
	Store                *Store
	Runner               *Runner
	Auth                 func(*http.Request) (string, error)
	CanCreateWorkspace   func(string) bool
	Connections          []Connection
	Lookup               func(string) (string, error)
	mu                   sync.Mutex
	runs                 map[string]context.CancelFunc
	subscribers          map[string]map[chan struct{}]bool
	wg                   sync.WaitGroup
	closed               bool
	streamsStopped       bool
}

func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	s.streamsStopped = true
	for _, clients := range s.subscribers {
		for ch := range clients {
			close(ch)
		}
	}
	s.subscribers = nil
	for _, cancel := range s.runs {
		cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
	s.Store.Close()
}
func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, err error) {
	respond(w, status, map[string]string{"error": err.Error()})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fail(w, 400, errors.New("invalid request body"))
		return false
	}
	return true
}
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, err := s.Auth(r)
	if err != nil {
		fail(w, 401, errors.New("sign in with your Fathom token"))
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/board"), "/")
	parts := strings.Split(path, "/")
	if path == "" {
		if r.Method == "GET" {
			workspaces, err := s.Store.Workspaces(user)
			if err != nil {
				fail(w, 500, err)
				return
			}
			models := []string{}
			ready := ""
			if err := s.Runner.Ready(); err != nil {
				ready = err.Error()
			} else {
				models = s.Runner.Router.Names()
			}
			respond(w, 200, map[string]any{"workspaces": workspaces, "models": models, "runSetup": ready, "userId": user})
			return
		}
		if r.Method == "POST" {
			if !s.CanCreateWorkspace(user) {
				fail(w, 403, ErrForbidden)
				return
			}
			var in struct {
				Name string `json:"name"`
			}
			if !decode(w, r, &in) {
				return
			}
			ws, err := s.Store.CreateWorkspace(in.Name, user)
			if err != nil {
				fail(w, 400, err)
				return
			}
			respond(w, 201, ws)
			return
		}
		fail(w, 405, errors.New("GET or POST required"))
		return
	}
	if path == "projects" {
		s.uploadProject(w, r, user)
		return
	}
	if path == "demo" {
		s.createDemo(w, r, user)
		return
	}
	workspace := parts[0]
	role := s.Store.Role(workspace, user)
	if role == "" {
		fail(w, 404, errors.New("workspace not found"))
		return
	}
	if len(parts) == 2 && parts[1] == "files" {
		s.browseFiles(w, r, workspace, user)
		return
	}
	if len(parts) >= 2 && parts[1] == "connections" {
		action := ""
		if len(parts) == 3 {
			action = parts[2]
		} else if len(parts) != 2 {
			fail(w, 404, errors.New("route not found"))
			return
		}
		s.manageConnections(w, r, workspace, user, action)
		return
	}
	if r.Method == "GET" && len(parts) == 2 && parts[1] == "events" {
		s.stream(w, r, workspace, user)
		return
	}
	if r.Method == "GET" && len(parts) == 1 {
		tasks, err := s.Store.Tasks(workspace)
		if err != nil {
			fail(w, 500, err)
			return
		}
		members, err := s.Store.Members(workspace)
		if err != nil {
			fail(w, 500, err)
			return
		}
		events, err := s.Store.Activity(workspace)
		if err != nil {
			fail(w, 500, err)
			return
		}
		records, err := s.connectionRecords(workspace)
		if err != nil {
			fail(w, 500, errors.New("connections unavailable"))
			return
		}
		connections := []Connection{}
		for _, c := range records {
			if c.Enabled {
				connections = append(connections, c.Config)
			}
		}
		manage := role == "admin" && s.CanManageConnections != nil && s.CanManageConnections(r, user)
		respond(w, 200, map[string]any{"tasks": tasks, "members": members, "activity": events, "role": role, "connections": connections, "canManageConnections": manage})
		return
	}
	if len(parts) == 2 && parts[1] == "archive" && r.Method == "GET" {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		buffer := &limitedBuffer{max: 32 << 20}
		if err := s.Runner.Archive(ctx, workspace, buffer); err != nil {
			fail(w, 503, errors.New("workspace archive unavailable; check the container image and Docker service"))
			return
		}
		if buffer.Len() >= 32<<20 {
			fail(w, 413, errors.New("workspace archive exceeds the 32 MB download limit"))
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="workspace.tar.gz"`)
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(buffer.Bytes())
		return
	}
	if r.Method == "GET" && len(parts) == 4 && parts[1] == "tasks" && parts[3] == "evidence" {
		t, err := s.Store.Task(workspace, parts[2])
		if err != nil {
			fail(w, 404, errors.New("task not found"))
			return
		}
		records, err := s.Store.Evidence(workspace, t.ID)
		if err != nil {
			fail(w, 500, err)
			return
		}
		current := []Evidence{}
		for _, e := range records {
			if (e.Role == "builder" && e.Revision == t.BuildRevision) || (e.Role == "reviewer" && e.Revision == t.ReviewRevision) {
				current = append(current, e)
			}
		}
		respond(w, 200, current)
		return
	}
	if r.Method != "POST" {
		fail(w, 405, errors.New("POST required"))
		return
	}
	if role == "viewer" {
		fail(w, 403, ErrForbidden)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	role = s.Store.Role(workspace, user)
	if role != "member" && role != "admin" {
		fail(w, 403, ErrForbidden)
		return
	}
	if s.closed {
		fail(w, 503, errors.New("gateway is stopping"))
		return
	}
	if len(parts) == 2 {
		switch parts[1] {
		case "members":
			var in Member
			if !decode(w, r, &in) {
				return
			}
			if err = s.Store.SetMember(workspace, user, in.UserID, in.Role); err != nil {
				fail(w, 403, err)
				return
			}
			s.notifyLocked(workspace)
			respond(w, 200, map[string]bool{"ok": true})
			return
		case "tasks":
			var in struct {
				Title       string `json:"title"`
				Description string `json:"description"`
				Assignee    string `json:"assignee"`
				Builder     string `json:"builder"`
				Reviewer    string `json:"reviewer"`
			}
			if !decode(w, r, &in) {
				return
			}
			if in.Assignee != "" && s.Store.Role(workspace, in.Assignee) == "" {
				fail(w, 400, errors.New("assignee must be a workspace member"))
				return
			}
			t, err := s.Store.CreateTask(Task{WorkspaceID: workspace, Title: in.Title, Description: in.Description, Assignee: in.Assignee, Builder: in.Builder, Reviewer: in.Reviewer}, user)
			if err != nil {
				fail(w, 400, err)
				return
			}
			s.notifyLocked(workspace)
			respond(w, 201, t)
			return
		case "import":
			var in struct {
				Provider string `json:"provider"`
				ID       string `json:"id"`
			}
			if !decode(w, r, &in) {
				return
			}
			c, err := s.connector(workspace, in.Provider)
			if err != nil {
				fail(w, 500, errors.New("connections unavailable"))
				return
			}
			if c == nil {
				fail(w, 400, errors.New("connection is not configured for this workspace"))
				return
			}
			t, err := c.Import(r.Context(), in.ID)
			if err != nil {
				fail(w, 502, err)
				return
			}
			existing, err := s.Store.Tasks(workspace)
			if err != nil {
				fail(w, 500, err)
				return
			}
			for _, old := range existing {
				if old.Source == t.Source && old.ExternalID == t.ExternalID {
					respond(w, 200, old)
					return
				}
			}
			t.WorkspaceID = workspace
			t, err = s.Store.CreateTask(t, user)
			if err != nil {
				fail(w, 400, err)
				return
			}
			s.notifyLocked(workspace)
			respond(w, 201, t)
			return
		}
	}
	if len(parts) != 4 || parts[1] != "tasks" {
		fail(w, 404, errors.New("route not found"))
		return
	}
	t, err := s.Store.Task(workspace, parts[2])
	if err != nil {
		fail(w, 404, errors.New("task not found"))
		return
	}
	var in struct {
		Revision    int    `json:"revision"`
		Text        string `json:"text"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Assignee    string `json:"assignee"`
		Builder     string `json:"builder"`
		Reviewer    string `json:"reviewer"`
	}
	if !decode(w, r, &in) {
		return
	}
	if parts[3] != "comment" && in.Revision != t.Revision {
		fail(w, 409, ErrConflict)
		return
	}
	if len(in.Text) > 64000 {
		fail(w, 400, errors.New("comment too long"))
		return
	}
	action := parts[3]
	if action == "comment" {
		if err = s.Store.AddComment(workspace, t.ID, user, in.Text); err != nil {
			fail(w, 400, err)
			return
		}
		s.notifyLocked(workspace)
		respond(w, 201, map[string]string{"status": "Comment added."})
		return
	}
	if t.State == "running" {
		if action != "cancel" {
			fail(w, 409, errors.New("wait for the active run or cancel it first"))
			return
		}
		if cancel := s.runs[t.ID]; cancel != nil {
			cancel()
		}
		respond(w, 202, map[string]string{"status": "cancelling"})
		return
	}
	switch action {
	case "edit":
		if in.Assignee != "" && s.Store.Role(workspace, in.Assignee) == "" {
			fail(w, 400, errors.New("assignee must be a workspace member"))
			return
		}
		t.Title = in.Title
		t.Description = in.Description
		t.Assignee = in.Assignee
		t.Builder = in.Builder
		t.Reviewer = in.Reviewer
		t.BuildRevision = 0
		t.ReviewRevision = 0
		t.Handoff = ""
		t.Review = ""
		t.State = "backlog"
	case "ready":
		t.State = "ready"
	case "changes":
		t.State = "ready"
		t.Review += "\nHuman requested changes: " + in.Text
	case "approve":
		if t.State != "review" || t.Review == "" {
			fail(w, 409, errors.New("run the reviewer before approving"))
			return
		}
		t.State = "done"
	case "build", "review":
		// Membership grants only the isolated workspace runner. It must not require
		// the global operator role, which also grants host-agent execution.
		if err = s.Runner.Ready(); err != nil {
			fail(w, 503, err)
			return
		}
		if action == "review" && t.Handoff == "" {
			fail(w, 409, errors.New("a builder handoff is required"))
			return
		}
		if len(s.runs) >= 4 {
			fail(w, 429, errors.New("four agent runs are already active"))
			return
		}
		model := t.Builder
		if action == "review" {
			model = t.Reviewer
		}
		if _, err = s.Runner.Router.Get(model); err != nil {
			fail(w, 400, err)
			return
		}
		previousReview := t.Review
		t.State = "running"
		t.Review = ""
		if action == "build" {
			t.BuildRevision = t.Revision + 1
			t.ReviewRevision = 0
		} else {
			t.ReviewRevision = t.Revision + 1
		}
		t, err = s.Store.Save(t, user, action+"_started", in.Text)
		if err != nil {
			fail(w, 409, err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		if s.runs == nil {
			s.runs = map[string]context.CancelFunc{}
		}
		s.runs[t.ID] = cancel
		s.wg.Add(1)
		go s.run(ctx, cancel, t, action == "review", user, previousReview)
		s.notifyLocked(workspace)
		respond(w, 202, t)
		return
	case "publish":
		c, err := s.connector(workspace, t.Source)
		if err != nil {
			fail(w, 500, errors.New("connections unavailable"))
			return
		}
		if c == nil {
			fail(w, 400, errors.New("linked connection is unavailable"))
			return
		}
		text := "Fathom team handoff — " + t.Title + "\nState: " + t.State + "\n\n" + t.Handoff + "\n\nReview:\n" + t.Review
		if t.Handoff == "" {
			fail(w, 400, errors.New("no handoff to publish"))
			return
		}
		if err = c.Publish(r.Context(), t, text); err != nil {
			fail(w, 502, err)
			return
		}
		in.Text = "Published handoff to " + t.Source
	default:
		fail(w, 404, errors.New("unknown task action"))
		return
	}
	t, err = s.Store.Save(t, user, action, in.Text)
	if err != nil {
		fail(w, 409, err)
		return
	}
	s.notifyLocked(workspace)
	respond(w, 200, t)
}
func (s *Service) connector(workspace, provider string) (*Connector, error) {
	records, err := s.connectionRecords(workspace)
	if err != nil {
		return nil, err
	}
	for _, c := range records {
		if c.Enabled && c.Config.Provider == provider {
			return &Connector{Config: c.Config, Lookup: s.Lookup, Client: s.ConnectionClient}, nil
		}
	}
	return nil, nil
}
func (s *Service) run(ctx context.Context, cancel context.CancelFunc, t Task, review bool, user, previousReview string) {
	defer s.wg.Done()
	defer cancel()
	promptTask := t
	promptTask.Review = previousReview
	result, err := s.Runner.runWithProgress(ctx, promptTask, review, func(c CommandRecord) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return
		}
		role := "Builder"
		if review {
			role = "Reviewer"
		}
		command := strings.SplitN(c.Command, "\n", 2)[0]
		if len(command) > 160 {
			command = command[:160] + "…"
		}
		text := fmt.Sprintf("%s %s finished with exit %d (%.1fs): %s", role, c.Kind, c.ExitCode, float64(c.DurationMS)/1000, command)
		if err := s.Store.AddProgress(t.WorkspaceID, t.ID, "agent:"+user, text); err != nil {
			slog.Error("failed to save run activity", "task", t.ID, "err", err)
			return
		}
		s.notifyLocked(t.WorkspaceID)
	}, func() bool {
		role := s.Store.Role(t.WorkspaceID, user)
		return role == "member" || role == "admin"
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.runs, t.ID)
	kind := "handoff"
	text := result.Handoff
	if saveErr := s.Store.SaveEvidence(t.WorkspaceID, t.ID, result.Evidence); saveErr != nil {
		err = errors.New("could not persist run evidence; inspect workspace before retrying")
	}
	t.State = "review"
	if err != nil {
		if !review {
			t.Handoff = ""
		}
		t.State = "blocked"
		kind = "run_failed"
		text = err.Error()
	} else if review {
		kind = "reviewed"
		t.Review = result.Handoff
	} else {
		t.Handoff = result.Handoff
	}
	if _, err := s.Store.Save(t, "agent:"+user, kind, text); err != nil {
		slog.Error("failed to persist task outcome", "task", t.ID, "err", err)
	} else {
		s.notifyLocked(t.WorkspaceID)
	}
}
