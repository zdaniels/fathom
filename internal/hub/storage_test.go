package hub

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mkMeta(name, version string, published time.Time) Metadata {
	return Metadata{
		Name:        name,
		Version:     version,
		Description: "test skill",
		Author:      "tester",
		Tags:        []string{"test", "demo"},
		PublishedAt: published,
	}
}

func TestStorageStoreAndGet(t *testing.T) {
	s, err := NewStorage(t.TempDir())
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	meta := mkMeta("alpha", "1.0.0", time.Now().UTC())
	if err := s.Store("alpha", "1.0.0", []byte("tarball-bytes"), meta); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, ok := s.GetMetadata("alpha", "1.0.0")
	if !ok || got.Description != "test skill" {
		t.Errorf("GetMetadata = %+v ok=%v", got, ok)
	}
	data, err := s.Download("alpha", "1.0.0")
	if err != nil || string(data) != "tarball-bytes" {
		t.Errorf("Download = %q err=%v", data, err)
	}
}

func TestStorageRequiresNameAndVersion(t *testing.T) {
	s, _ := NewStorage(t.TempDir())
	if err := s.Store("", "1.0.0", nil, Metadata{}); err == nil {
		t.Error("empty name should error")
	}
	if err := s.Store("x", "", nil, Metadata{}); err == nil {
		t.Error("empty version should error")
	}
}

func TestStorageListReturnsLatestVersion(t *testing.T) {
	s, _ := NewStorage(t.TempDir())
	older := time.Now().Add(-2 * time.Hour).UTC()
	newer := time.Now().Add(-1 * time.Hour).UTC()
	_ = s.Store("alpha", "1.0.0", []byte("v1"), mkMeta("alpha", "1.0.0", older))
	_ = s.Store("alpha", "2.0.0", []byte("v2"), mkMeta("alpha", "2.0.0", newer))
	list := s.List()
	if len(list) != 1 || list[0].Version != "2.0.0" {
		t.Errorf("List = %+v, want single entry at 2.0.0", list)
	}
}

func TestStorageListSortsByMostRecent(t *testing.T) {
	s, _ := NewStorage(t.TempDir())
	now := time.Now().UTC()
	_ = s.Store("old", "1", []byte("o"), mkMeta("old", "1", now.Add(-3*time.Hour)))
	_ = s.Store("mid", "1", []byte("m"), mkMeta("mid", "1", now.Add(-2*time.Hour)))
	_ = s.Store("new", "1", []byte("n"), mkMeta("new", "1", now.Add(-time.Hour)))
	list := s.List()
	if len(list) != 3 || list[0].Name != "new" || list[2].Name != "old" {
		t.Errorf("expected new→mid→old order, got %v", list)
	}
}

func TestStorageIncrementDownloadPersists(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStorage(dir)
	_ = s.Store("alpha", "1.0.0", []byte("x"), mkMeta("alpha", "1.0.0", time.Now().UTC()))
	s.IncrementDownload("alpha", "1.0.0")
	s.IncrementDownload("alpha", "1.0.0")
	// Re-open from disk to confirm persistence.
	s2, _ := NewStorage(dir)
	got, _ := s2.GetMetadata("alpha", "1.0.0")
	if got.Downloads != 2 {
		t.Errorf("Downloads = %d after two increments, want 2", got.Downloads)
	}
}

func TestStorageIncrementDownloadNoOpForMissing(t *testing.T) {
	s, _ := NewStorage(t.TempDir())
	// Should not panic — silent no-op.
	s.IncrementDownload("nope", "0.0.0")
}

func TestStorageAtomicWriteCleansTmp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.bin")
	if err := atomicWrite(target, []byte("hi"), 0o644); err != nil {
		t.Fatalf("atomicWrite: %v", err)
	}
	// .tmp should not linger after rename.
	if _, err := os.Stat(target + ".tmp"); err == nil {
		t.Error(".tmp file should not exist after atomic rename")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "hi" {
		t.Errorf("target = %q err=%v", data, err)
	}
}
