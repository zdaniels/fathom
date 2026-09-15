package collab

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const projectLimit = 16 << 20

func safeProjectPath(name string) bool {
	if name == "" || len(name) > 1024 || !utf8.ValidString(name) || strings.ContainsAny(name, "\\:") || strings.HasPrefix(name, "/") || path.Clean(name) != name || name == "." || name == ".." || strings.HasPrefix(name, "../") {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Rebuild archives from validated regular files; never extract user ZIP metadata.
func readProjectZIP(data []byte) ([]projectFile, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errors.New("upload a valid ZIP archive")
	}
	if len(zr.File) > 400 {
		return nil, errors.New("ZIP exceeds 400 entries")
	}
	files := []projectFile{}
	seen := map[string]bool{}
	total := 0
	for _, f := range zr.File {
		name := strings.TrimSuffix(f.Name, "/")
		if !safeProjectPath(name) || seen[name] {
			return nil, errors.New("ZIP contains an unsafe or duplicate path")
		}
		seen[name] = true
		if f.FileInfo().IsDir() {
			continue
		}
		if !f.Mode().IsRegular() {
			return nil, errors.New("ZIP may contain only regular files and directories")
		}
		if len(files) >= 200 || f.UncompressedSize64 > uint64(projectLimit-total) {
			return nil, errors.New("project exceeds 200 files or 16 MB expanded")
		}
		rc, err := f.Open()
		if err != nil {
			return nil, errors.New("ZIP file could not be read")
		}
		body, err := io.ReadAll(io.LimitReader(rc, int64(projectLimit-total+1)))
		rc.Close()
		if err != nil || len(body) > projectLimit-total {
			return nil, errors.New("ZIP is corrupt or exceeds 16 MB expanded")
		}
		total += len(body)
		mode := int64(0644)
		if f.Mode()&0111 != 0 {
			mode = 0755
		}
		files = append(files, projectFile{Name: name, Body: body, Mode: mode})
	}
	if len(files) == 0 {
		return nil, errors.New("ZIP contains no files")
	}
	// Reject a file used as a parent directory, regardless of archive order.
	regular := map[string]bool{}
	for _, f := range files {
		regular[f.Name] = true
	}
	for _, f := range files {
		for parent := path.Dir(f.Name); parent != "."; parent = path.Dir(parent) {
			if regular[parent] {
				return nil, errors.New("ZIP contains conflicting file and directory paths")
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, nil
}
func (s *Service) uploadProject(w http.ResponseWriter, r *http.Request, user string) {
	if r.Method != "POST" {
		fail(w, 405, errors.New("POST required"))
		return
	}
	if s.CanCreateWorkspace == nil || !s.CanCreateWorkspace(user) {
		fail(w, 403, ErrForbidden)
		return
	}
	if !s.uploadMu.TryLock() {
		fail(w, 409, errors.New("another upload is being processed; try again shortly"))
		return
	}
	defer s.uploadMu.Unlock()
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" || len(name) > 100 {
		fail(w, 400, errors.New("workspace name must be 1–100 characters"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, projectLimit)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		fail(w, 413, errors.New("ZIP upload exceeds 16 MB or was interrupted"))
		return
	}
	files, err := readProjectZIP(data)
	if err != nil {
		fail(w, 400, err)
		return
	}
	s.finishProject(w, r, user, name, files, nil)
}

type workspaceFile struct {
	Path    string `json:"path"`
	Text    string `json:"text"`
	Preview bool   `json:"preview"`
}

func (r *Runner) files(ctx context.Context, workspace string) ([]workspaceFile, error) {
	compressed := &limitedBuffer{max: projectLimit + 1}
	if err := r.Archive(ctx, workspace, compressed); err != nil {
		return nil, errors.New("workspace files unavailable; check Docker and collaboration.image")
	}
	if compressed.Len() > projectLimit {
		return nil, errors.New("workspace exceeds the 16 MB browsing limit; use archive download")
	}
	gz, err := gzip.NewReader(bytes.NewReader(compressed.Bytes()))
	if err != nil {
		return nil, errors.New("workspace archive could not be read")
	}
	defer gz.Close()
	data, err := io.ReadAll(io.LimitReader(gz, projectLimit+1))
	if err != nil || len(data) > projectLimit {
		return nil, errors.New("workspace exceeds the 16 MB browsing limit; use archive download")
	}
	snapshot, err := readSnapshot(data)
	if err != nil {
		return nil, err
	}
	files := []workspaceFile{}
	for name, f := range snapshot {
		files = append(files, workspaceFile{Path: name, Text: f.text, Preview: f.preview})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}
func (s *Service) browseFiles(w http.ResponseWriter, r *http.Request, workspace, user string) {
	if r.Method != "GET" {
		fail(w, 405, errors.New("GET required"))
		return
	}
	if !s.filesMu.TryLock() {
		fail(w, 409, errors.New("another file snapshot is loading; try again shortly"))
		return
	}
	defer s.filesMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	files, err := s.Runner.files(ctx, workspace)
	if err != nil {
		fail(w, 503, err)
		return
	}
	if s.Store.Role(workspace, user) == "" {
		fail(w, 403, ErrForbidden)
		return
	}
	respond(w, 200, files)
}
