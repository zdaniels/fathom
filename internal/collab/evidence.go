package collab

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Evidence is coordinator-recorded execution data, not a model's description.
// It is stored separately so polling the board never transfers file contents.
type Evidence struct {
	Revision   int             `json:"revision"`
	Role       string          `json:"role"`
	StartedAt  time.Time       `json:"startedAt"`
	FinishedAt time.Time       `json:"finishedAt"`
	Changes    []FileChange    `json:"changes"`
	Commands   []CommandRecord `json:"commands"`
	Error      string          `json:"error,omitempty"`
	Warning    string          `json:"warning,omitempty"`
}
type FileChange struct {
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	Before     string `json:"before"`
	After      string `json:"after"`
	Preview    bool   `json:"preview"`
	BeforeMode int64  `json:"beforeMode"`
	AfterMode  int64  `json:"afterMode"`
}
type CommandRecord struct {
	Kind       string `json:"kind"`
	Command    string `json:"command"`
	Output     string `json:"output"`
	ExitCode   int    `json:"exitCode"`
	DurationMS int64  `json:"durationMs"`
	Truncated  bool   `json:"truncated"`
}
type RunResult struct {
	Handoff  string
	Evidence Evidence
}
type snapshotFile struct {
	hash, text string
	preview    bool
	mode       int64
}

func readSnapshot(data []byte) (map[string]snapshotFile, error) {
	files := map[string]snapshotFile{}
	tr := tar.NewReader(bytes.NewReader(data))
	previewBytes := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		if len(files) >= 200 {
			return nil, fmt.Errorf("workspace exceeds 200 files")
		}
		name := strings.TrimPrefix(h.Name, "./")
		if len(name) > 1024 {
			return nil, fmt.Errorf("file path exceeds preview limit")
		}
		// Never extract archives or follow symlinks. Include type, target and mode
		// in fingerprints so binary, link and permission changes remain visible.
		hash := sha256.New()
		fmt.Fprintf(hash, "%d:%d:%s:", h.Typeflag, h.Mode, h.Linkname)
		body, err := io.ReadAll(io.LimitReader(tr, (16<<20)+1))
		if err != nil || len(body) > 16<<20 {
			return nil, fmt.Errorf("file exceeds snapshot limit")
		}
		hash.Write(body)
		preview := h.Typeflag == tar.TypeReg && len(body) <= 32768 && utf8.Valid(body) && !bytes.ContainsRune(body, 0)
		preview = preview && previewBytes+len(body) <= 1<<20
		text := ""
		if preview {
			previewBytes += len(body)
			text = string(body)
		}
		files[name] = snapshotFile{hex.EncodeToString(hash.Sum(nil)), text, preview, h.Mode}
	}
}
func snapshot(ctx context.Context, container string) (map[string]snapshotFile, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "exec", container, "tar", "-cf", "-", "-C", "/workspace", ".")
	out := &limitedBuffer{max: (16 << 20) + 1}
	cmd.Stdout = out
	cmd.Stderr = &limitedBuffer{max: 4096}
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("workspace snapshot unavailable")
	}
	if out.Len() > 16<<20 {
		return nil, fmt.Errorf("workspace snapshot exceeds 16 MB")
	}
	return readSnapshot(out.Bytes())
}
func changes(before, after map[string]snapshotFile) []FileChange {
	names := map[string]bool{}
	for n := range before {
		names[n] = true
	}
	for n := range after {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	out := []FileChange{}
	for _, n := range sorted {
		a, had := before[n]
		b, has := after[n]
		if had && has && a.hash == b.hash {
			continue
		}
		kind := "modified"
		if !had {
			kind = "added"
		}
		if !has {
			kind = "deleted"
		}
		out = append(out, FileChange{Path: n, Kind: kind, Before: a.text, After: b.text, Preview: (!had || a.preview) && (!has || b.preview), BeforeMode: a.mode, AfterMode: b.mode})
	}
	return out
}
func (s *Store) SaveEvidence(workspace, task string, evidence Evidence) error {
	data, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO run_evidence(workspace_id,task_id,role,record) VALUES(?,?,?,?) ON CONFLICT(workspace_id,task_id,role) DO UPDATE SET record=excluded.record`, workspace, task, evidence.Role, data)
	return err
}
func (s *Store) Evidence(workspace, task string) ([]Evidence, error) {
	rows, err := s.db.Query(`SELECT record FROM run_evidence WHERE workspace_id=? AND task_id=? ORDER BY role`, workspace, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Evidence{}
	for rows.Next() {
		var data []byte
		var e Evidence
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(data, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
