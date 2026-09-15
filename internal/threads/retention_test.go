package threads

import (
	"path/filepath"
	"testing"
	"time"
)

func TestPurgeOnlyExpiredTrashCascadesMessages(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "threads.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old, _ := s.Create("user", "old")
	live, _ := s.Create("user", "live")
	_, _ = s.Append(old.ID, "user", "uniqueoldtext", "", nil)
	_, _ = s.Append(live.ID, "user", "live", "", nil)
	if err = s.SoftDelete(old.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PurgeDeletedBefore(time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if n, err := s.PurgeDeletedBefore(time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	var n int
	if err = s.db.QueryRow("SELECT count(*) FROM messages WHERE thread_id=?", old.ID).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if _, err = s.Get(live.ID); err != nil {
		t.Fatal("purged live thread", err)
	}
}
