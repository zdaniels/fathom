package collab

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCommentsDuringRunDoNotLoseOutcome(t *testing.T) {
	store := newStore(t)
	workspace, _ := store.CreateWorkspace("Live", "alice")
	task, _ := store.CreateTask(Task{WorkspaceID: workspace.ID, Title: "Working"}, "alice")
	task.State = "running"
	task, _ = store.Save(task, "alice", "build_started", "")
	svc := &Service{Store: store, Auth: func(r *http.Request) (string, error) { return r.Header.Get("User"), nil }}
	post := func(user, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v1/board/"+workspace.ID+"/tasks/"+task.ID+"/comment", strings.NewReader(body))
		r.Header.Set("User", user)
		w := httptest.NewRecorder()
		svc.ServeHTTP(w, r)
		return w
	}
	if got := post("alice", `{"revision":1,"text":"Comment while running"}`); got.Code != 201 {
		t.Fatal(got.Body.String())
	}
	if got := post("alice", `{"text":"   "}`); got.Code != 400 {
		t.Fatal("empty comment accepted")
	}
	if err := store.SetMember(workspace.ID, "alice", "bob", "viewer"); err != nil {
		t.Fatal(err)
	}
	if got := post("bob", `{"text":"forbidden"}`); got.Code != 403 {
		t.Fatal("viewer comment accepted")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.AddComment(workspace.ID, task.ID, "alice", "Concurrent comment"); err != nil {
				t.Error(err)
			}
		}()
	}
	task.State = "review"
	task.Handoff = "Finished successfully"
	saved, err := store.Save(task, "agent:alice", "handoff", task.Handoff)
	if err != nil {
		t.Fatal("comments broke run CAS", err)
	}
	wg.Wait()
	current, err := store.Task(workspace.ID, task.ID)
	if err != nil || current.Handoff != task.Handoff || current.Revision != saved.Revision {
		t.Fatal("lost outcome", current, err)
	}
	events, _ := store.Activity(workspace.ID)
	comments := 0
	for _, e := range events {
		if e.Kind == "comment" {
			comments++
		}
	}
	if comments != 9 {
		t.Fatalf("got %d comments", comments)
	}
}

func TestLiveUpdatesReconnectRevocationAndShutdown(t *testing.T) {
	store := newStore(t)
	workspace, _ := store.CreateWorkspace("Live", "alice")
	task, _ := store.CreateTask(Task{WorkspaceID: workspace.ID, Title: "Demo"}, "alice")
	if err := store.SetMember(workspace.ID, "alice", "bob", "member"); err != nil {
		t.Fatal(err)
	}
	svc := &Service{Store: store, Auth: func(r *http.Request) (string, error) { return r.Header.Get("User"), nil }}
	server := httptest.NewServer(svc)
	defer server.Close()
	defer svc.StopStreams()
	client := &http.Client{Timeout: 3 * time.Second}
	connect := func(user string) (*http.Response, *bufio.Reader) {
		req, _ := http.NewRequest("GET", server.URL+"/api/v1/board/"+workspace.ID+"/events", nil)
		req.Header.Set("User", user)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res, bufio.NewReader(res.Body)
	}
	frame := func(reader *bufio.Reader) string {
		var out strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			out.WriteString(line)
			if line == "\n" {
				return out.String()
			}
		}
	}
	denied, _ := connect("outsider")
	if denied.StatusCode != 404 {
		t.Fatal("nonmember subscribed")
	}
	denied.Body.Close()
	res, reader := connect("bob")
	defer res.Body.Close()
	if !strings.Contains(frame(reader), "event: changed") {
		t.Fatal("missing initial refresh")
	}
	req := httptest.NewRequest("POST", "/api/v1/board/"+workspace.ID+"/tasks/"+task.ID+"/comment", strings.NewReader(`{"text":"Live hello"}`))
	req.Header.Set("User", "alice")
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	if !strings.Contains(frame(reader), "event: changed") {
		t.Fatal("missing comment refresh")
	}
	res.Body.Close()
	// New subscribers always fetch a snapshot, including changes missed offline.
	res, reader = connect("bob")
	defer res.Body.Close()
	frame(reader)
	snapshotReq := httptest.NewRequest("GET", "/api/v1/board/"+workspace.ID, nil)
	snapshotReq.Header.Set("User", "bob")
	snapshot := httptest.NewRecorder()
	svc.ServeHTTP(snapshot, snapshotReq)
	var data struct{ Activity []Activity }
	if json.Unmarshal(snapshot.Body.Bytes(), &data) != nil {
		t.Fatal("invalid snapshot")
	}
	found := false
	for _, e := range data.Activity {
		found = found || e.Text == "Live hello"
	}
	if !found {
		t.Fatal("reconnect missed comment")
	}
	revoke := httptest.NewRequest("POST", "/api/v1/board/"+workspace.ID+"/members", strings.NewReader(`{"userId":"bob","role":""}`))
	revoke.Header.Set("User", "alice")
	revoked := httptest.NewRecorder()
	svc.ServeHTTP(revoked, revoke)
	if revoked.Code != 200 {
		t.Fatal(revoked.Body.String())
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		t.Fatal("revoked stream did not end", err)
	}
	res, reader = connect("alice")
	defer res.Body.Close()
	frame(reader)
	svc.StopStreams()
	if _, err := reader.ReadByte(); err != io.EOF {
		t.Fatal("shutdown did not end stream", err)
	}
	denied, _ = connect("alice")
	defer denied.Body.Close()
	if denied.StatusCode != 503 {
		t.Fatal("new stream accepted after shutdown")
	}
}
