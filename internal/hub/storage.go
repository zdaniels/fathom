// Package hub is the skill-registry server: HTTP API for discovering /
// publishing skills + the static frontend at hub-web/. Runs alongside the
// gateway, optionally; team/enterprise deployments often centralise a hub
// for the org while every developer's `fathom chat` queries it for skills.
package hub

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Metadata is the per-skill record kept in the registry index.
type Metadata struct {
	Name        string                 `json:"name"`
	Version     string                 `json:"version"`
	Description string                 `json:"description"`
	Author      string                 `json:"author"`
	Category    string                 `json:"category"`
	Tags        []string               `json:"tags"`
	Signature   string                 `json:"signature"`
	Verified    bool                   `json:"verified"`
	Permissions map[string]interface{} `json:"permissions"`
	Downloads   int                    `json:"downloads"`
	PublishedAt time.Time              `json:"publishedAt"`
	Readme      string                 `json:"readme,omitempty"`
}

// Storage persists skill tarballs + metadata to disk.
type Storage struct {
	mu      sync.RWMutex
	dataDir string
}

// NewStorage opens (or creates) the on-disk store at dataDir.
func NewStorage(dataDir string) (*Storage, error) {
	if err := os.MkdirAll(filepath.Join(dataDir, "metadata"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "blobs"), 0o755); err != nil {
		return nil, err
	}
	return &Storage{dataDir: dataDir}, nil
}

// Store writes both the tarball and the metadata for a given (name,version).
// Atomically via .tmp + rename.
func (s *Storage) Store(name, version string, tarball []byte, meta Metadata) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == "" || version == "" {
		return errors.New("name and version required")
	}
	if meta.PublishedAt.IsZero() {
		meta.PublishedAt = time.Now().UTC()
	}
	blobPath := filepath.Join(s.dataDir, "blobs", name+"-"+version+".tar.gz")
	if err := atomicWrite(blobPath, tarball, 0o644); err != nil {
		return err
	}
	metaPath := filepath.Join(s.dataDir, "metadata", name+".json")
	all := s.readAllVersionsLocked(name)
	all[version] = meta
	body, _ := json.MarshalIndent(all, "", "  ")
	return atomicWrite(metaPath, body, 0o644)
}

// GetMetadata returns the metadata for (name,version), if it exists.
func (s *Storage) GetMetadata(name, version string) (Metadata, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all := s.readAllVersionsLocked(name)
	m, ok := all[version]
	return m, ok
}

// Download returns the tarball bytes for (name,version).
func (s *Storage) Download(name, version string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	path := filepath.Join(s.dataDir, "blobs", name+"-"+version+".tar.gz")
	return os.ReadFile(path)
}

// List returns the most recent version of every skill.
func (s *Storage) List() []Metadata {
	s.mu.RLock()
	defer s.mu.RUnlock()
	dir := filepath.Join(s.dataDir, "metadata")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Metadata
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			name := e.Name()[:len(e.Name())-len(".json")]
			versions := s.readAllVersionsLocked(name)
			latest := pickLatest(versions)
			if latest != nil {
				out = append(out, *latest)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PublishedAt.After(out[j].PublishedAt) })
	return out
}

// IncrementDownload bumps the counter for (name, version) atomically.
func (s *Storage) IncrementDownload(name, version string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.readAllVersionsLocked(name)
	m, ok := all[version]
	if !ok {
		return
	}
	m.Downloads++
	all[version] = m
	body, _ := json.MarshalIndent(all, "", "  ")
	metaPath := filepath.Join(s.dataDir, "metadata", name+".json")
	_ = atomicWrite(metaPath, body, 0o644)
}

func (s *Storage) readAllVersionsLocked(name string) map[string]Metadata {
	out := map[string]Metadata{}
	metaPath := filepath.Join(s.dataDir, "metadata", name+".json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(data, &out)
	return out
}

func pickLatest(versions map[string]Metadata) *Metadata {
	var best *Metadata
	for _, v := range versions {
		v := v
		if best == nil || v.PublishedAt.After(best.PublishedAt) {
			best = &v
		}
	}
	return best
}

func atomicWrite(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
