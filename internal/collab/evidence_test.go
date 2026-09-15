package collab

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func snapshotArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestEvidenceFileChangesAndLimits(t *testing.T) {
	before, err := readSnapshot(snapshotArchive(t, map[string]string{"keep": "same", "edit": "old\n", "gone": "remove", "binary": "a\x00b"}))
	if err != nil {
		t.Fatal(err)
	}
	after, err := readSnapshot(snapshotArchive(t, map[string]string{"keep": "same", "edit": "new\n", "added": "<script>alert(1)</script>", "binary": "c\x00d"}))
	if err != nil {
		t.Fatal(err)
	}
	diff := changes(before, after)
	if len(diff) != 4 || diff[0].Kind != "added" || diff[1].Preview || diff[2].Before != "old\n" || diff[2].After != "new\n" || diff[3].Kind != "deleted" {
		t.Fatalf("unexpected diff: %+v", diff)
	}
	large, err := readSnapshot(snapshotArchive(t, map[string]string{"large": strings.Repeat("x", 32769)}))
	if err != nil || large["large"].preview {
		t.Fatal("oversized preview", err)
	}
	files := map[string]string{}
	for i := 0; i < 201; i++ {
		files[fmt.Sprint(i)] = "x"
	}
	if _, err := readSnapshot(snapshotArchive(t, files)); err == nil {
		t.Fatal("file count limit not enforced")
	}
	if _, err := readSnapshot([]byte("not a tar")); err == nil {
		t.Fatal("malformed snapshot accepted")
	}
}
func TestEvidenceEndpointMembershipAndRunInvalidation(t *testing.T) {
	s := newStore(t)
	w, _ := s.CreateWorkspace("Demo", "alice")
	task, _ := s.CreateTask(Task{WorkspaceID: w.ID, Title: "Demo"}, "alice")
	task.BuildRevision = 2
	task, _ = s.Save(task, "alice", "build_started", "")
	evidence := Evidence{Role: "builder", Revision: 2, Changes: []FileChange{{Path: "demo.txt", Kind: "added", After: "hello", Preview: true}}}
	if err := s.SaveEvidence(w.ID, task.ID, evidence); err != nil {
		t.Fatal(err)
	}
	svc := &Service{Store: s, Auth: func(r *http.Request) (string, error) { return r.Header.Get("User"), nil }}
	read := func(user, workspace string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/v1/board/"+workspace+"/tasks/"+task.ID+"/evidence", nil)
		r.Header.Set("User", user)
		out := httptest.NewRecorder()
		svc.ServeHTTP(out, r)
		return out
	}
	if out := read("bob", w.ID); out.Code != 404 {
		t.Fatal("nonmember read evidence", out.Code)
	}
	if err := s.SetMember(w.ID, "alice", "bob", "viewer"); err != nil {
		t.Fatal(err)
	}
	out := read("bob", w.ID)
	var got []Evidence
	if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &got) != nil || len(got) != 1 || got[0].Changes[0].After != "hello" {
		t.Fatal(out.Body.String())
	}
	other, _ := s.CreateWorkspace("Other", "bob")
	if out = read("bob", other.ID); out.Code != 404 {
		t.Fatal("cross-workspace evidence exposed")
	}
	task.BuildRevision = 4
	if _, err := s.Save(task, "alice", "build_started", ""); err != nil {
		t.Fatal(err)
	}
	if out = read("bob", w.ID); out.Body.String() != "[]\n" {
		t.Fatal("old run shown during new build", out.Body.String())
	}
}
