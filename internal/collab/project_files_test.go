package collab

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func zipFixture(t *testing.T, entries []projectFile) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.Name, Method: zip.Deflate}
		h.SetMode(os.FileMode(e.Mode))
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write(e.Body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func TestProjectZIPValidation(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", "a/../../escape", "a\\escape", "C:/escape", "a/./b", "a//b", ".", "a\nfile"} {
		t.Run(name, func(t *testing.T) {
			if _, err := readProjectZIP(zipFixture(t, []projectFile{{name, []byte("x"), 0644}})); err == nil {
				t.Fatal("unsafe path accepted")
			}
		})
	}
	for _, entries := range [][]projectFile{
		{{"same", nil, 0644}, {"same", nil, 0644}},
		{{"dir/file", nil, 0644}, {"dir", nil, 0644}},
		{{"link", []byte("/etc/passwd"), int64(os.ModeSymlink | 0777)}},
		{{"bomb", bytes.Repeat([]byte("a"), projectLimit+1), 0644}},
	} {
		if _, err := readProjectZIP(zipFixture(t, entries)); err == nil {
			t.Fatal("unsafe archive accepted")
		}
	}
	entries := make([]projectFile, 201)
	for i := range entries {
		entries[i] = projectFile{Name: strings.Repeat("a", i+1), Mode: 0644}
	}
	if _, err := readProjectZIP(zipFixture(t, entries)); err == nil {
		t.Fatal("file limit bypassed")
	}
	valid := []projectFile{{"src/main.js", []byte("<script>alert('x')</script>"), 0644}, {"run.sh", []byte("echo ok"), 04755}, {"data.bin", []byte{0, 1, 2}, 0644}}
	files, err := readProjectZIP(zipFixture(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 || files[1].Mode != 0755 {
		t.Fatal("permissions were not normalized", files)
	}
	if _, err = readProjectZIP([]byte("invalid")); err == nil {
		t.Fatal("invalid ZIP accepted")
	}
}
func TestProjectFilePermissions(t *testing.T) {
	store := newStore(t)
	ws, _ := store.CreateWorkspace("Private", "alice")
	user := "bob"
	s := &Service{Store: store, Auth: func(*http.Request) (string, error) { return user, nil }, CanCreateWorkspace: func(string) bool { return false }}
	for _, p := range []string{"/api/v1/board/" + ws.ID + "/files", "/api/v1/board/projects?name=Upload"} {
		req := httptest.NewRequest("POST", p, strings.NewReader("not a ZIP"))
		if strings.HasSuffix(p, "/files") {
			req.Method = "GET"
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		if w.Code != 403 && w.Code != 404 {
			t.Fatal(w.Code)
		}
	}
	user = "alice"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/board/"+ws.ID+"/files", nil))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
}
func TestWorkspaceContainersProjectUpload(t *testing.T) {
	image := os.Getenv("FATHOM_TEST_WORKSPACE_IMAGE")
	if image == "" {
		t.Skip("set FATHOM_TEST_WORKSPACE_IMAGE")
	}
	store := newStore(t)
	user := "alice"
	s := &Service{Store: store, Runner: &Runner{Image: image}, Auth: func(*http.Request) (string, error) { return user, nil }, CanCreateWorkspace: func(string) bool { return true }}
	payload := zipFixture(t, []projectFile{{"src/example.txt", []byte("<script>literal text</script>"), 0644}, {"data.bin", []byte{0, 1}, 0644}})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/board/projects?name=Uploaded", bytes.NewReader(payload)))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var ws Workspace
	json.Unmarshal(w.Body.Bytes(), &ws)
	defer removeProjectVolume(ws.ID)
	if err := store.SetMember(ws.ID, "alice", "viewer", "viewer"); err != nil {
		t.Fatal(err)
	}
	user = "viewer"
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/board/"+ws.ID+"/files", nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var files []workspaceFile
	if err := json.Unmarshal(w.Body.Bytes(), &files); err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Preview || files[1].Text != "<script>literal text</script>" {
		t.Fatal(files)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Runner.seedProject(ctx, ws.ID, demoProject()); err == nil {
		t.Fatal("upload workspace was overwritten")
	}
	user = "outsider"
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/board/"+ws.ID+"/files", nil))
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
}
