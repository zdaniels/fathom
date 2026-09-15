package threads

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "threads.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestCreateListGet(t *testing.T) {
	s := newTestStore(t)
	a, err := s.Create("alice", "Trip planning")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.ID == "" || a.CreatedAt.IsZero() {
		t.Fatalf("bad thread: %+v", a)
	}
	got, err := s.Get(a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "Trip planning" || got.UserID != "alice" {
		t.Errorf("get: %+v", got)
	}
	list, err := s.List("alice", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != a.ID {
		t.Errorf("list: got %d items", len(list))
	}
}

func TestListNewestFirst(t *testing.T) {
	s := newTestStore(t)
	a, _ := s.Create("alice", "first")
	time.Sleep(10 * time.Millisecond)
	b, _ := s.Create("alice", "second")
	time.Sleep(10 * time.Millisecond)
	c, _ := s.Create("alice", "third")
	list, err := s.List("alice", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("len=%d", len(list))
	}
	// Newest first; touching b later via Append should bump it to top.
	if list[0].ID != c.ID || list[1].ID != b.ID || list[2].ID != a.ID {
		t.Fatalf("initial order wrong: %v", []string{list[0].ID, list[1].ID, list[2].ID})
	}
	if _, err := s.Append(b.ID, "user", "ping", "", nil); err != nil {
		t.Fatalf("append: %v", err)
	}
	list, _ = s.List("alice", 0)
	if list[0].ID != b.ID {
		t.Errorf("after append, b should be on top — got %s", list[0].ID)
	}
}

func TestListUserScoped(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Create("alice", "hers")
	_, _ = s.Create("bob", "his")
	alice, _ := s.List("alice", 0)
	bob, _ := s.List("bob", 0)
	if len(alice) != 1 || alice[0].Title != "hers" {
		t.Errorf("alice list: %v", alice)
	}
	if len(bob) != 1 || bob[0].Title != "his" {
		t.Errorf("bob list: %v", bob)
	}
}

func TestAppendAndTail(t *testing.T) {
	s := newTestStore(t)
	th, _ := s.Create("alice", "")
	for i, body := range []string{"hi", "hello", "how are you?", "good", "and you?"} {
		role := "user"
		if i%2 == 1 {
			role = "agent"
		}
		if _, err := s.Append(th.ID, role, body, "dev1", nil); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	msgs, err := s.MessagesTail(th.ID, 3)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages, got %d", len(msgs))
	}
	want := []string{"how are you?", "good", "and you?"}
	for i, m := range msgs {
		if m.Content != want[i] {
			t.Errorf("msg %d = %q, want %q", i, m.Content, want[i])
		}
	}
}

func TestAppendValidatesRole(t *testing.T) {
	s := newTestStore(t)
	th, _ := s.Create("alice", "")
	if _, err := s.Append(th.ID, "boss", "no good", "", nil); err == nil {
		t.Errorf("invalid role accepted")
	}
}

func TestAppendStoresMetadata(t *testing.T) {
	s := newTestStore(t)
	th, _ := s.Create("alice", "")
	meta := map[string]interface{}{
		"tool_calls": []string{"web.search"},
		"usage":      map[string]int{"prompt": 12, "completion": 34},
	}
	m, err := s.Append(th.ID, "agent", "the answer is 42", "", meta)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	roundtrip, _ := s.MessagesTail(th.ID, 1)
	if len(roundtrip) != 1 {
		t.Fatalf("expected 1 message back")
	}
	if roundtrip[0].ID != m.ID {
		t.Errorf("id mismatch")
	}
	if roundtrip[0].Metadata["tool_calls"] == nil {
		t.Errorf("metadata not roundtripped: %+v", roundtrip[0].Metadata)
	}
}

func TestMessagesAfterPagination(t *testing.T) {
	s := newTestStore(t)
	th, _ := s.Create("alice", "")
	var bookmark time.Time
	for i := 0; i < 5; i++ {
		m, _ := s.Append(th.ID, "user", "msg", "", nil)
		if i == 1 {
			bookmark = m.CreatedAt
		}
		time.Sleep(2 * time.Millisecond)
	}
	after, err := s.MessagesAfter(th.ID, bookmark, 0)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	// Strictly greater than bookmark → 3 messages (indices 2, 3, 4)
	if len(after) != 3 {
		t.Errorf("got %d after bookmark, want 3", len(after))
	}
}

func TestSoftDeleteHidesFromList(t *testing.T) {
	s := newTestStore(t)
	a, _ := s.Create("alice", "trash me")
	if err := s.SoftDelete(a.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	list, _ := s.List("alice", 0)
	if len(list) != 0 {
		t.Errorf("deleted thread still in list")
	}
	// Get still returns it (with DeletedAt set) — supports undo UX.
	got, err := s.Get(a.ID)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if got.DeletedAt == nil {
		t.Errorf("deleted_at not set on direct Get")
	}
	// Appending to a soft-deleted thread is refused.
	if _, err := s.Append(a.ID, "user", "ghost", "", nil); err == nil {
		t.Errorf("expected ErrDeleted on append to soft-deleted thread")
	}
}

func TestRenameBumpsUpdatedAt(t *testing.T) {
	s := newTestStore(t)
	a, _ := s.Create("alice", "old name")
	orig := a.UpdatedAt
	time.Sleep(10 * time.Millisecond)
	if err := s.Rename(a.ID, "new name"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	got, _ := s.Get(a.ID)
	if got.Title != "new name" {
		t.Errorf("title = %q, want 'new name'", got.Title)
	}
	if !got.UpdatedAt.After(orig) {
		t.Errorf("updated_at didn't move forward")
	}
}

func TestGetMissingThreadReturnsErrNotFound(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Get("no-such-id"); err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestRestoreUnDeletesSoftDeleted(t *testing.T) {
	s := newTestStore(t)
	a, _ := s.Create("alice", "")
	if err := s.SoftDelete(a.ID); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}
	if list, _ := s.List("alice", 0); len(list) != 0 {
		t.Fatalf("expected empty list after soft-delete")
	}
	if err := s.Restore(a.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if list, _ := s.List("alice", 0); len(list) != 1 || list[0].ID != a.ID {
		t.Errorf("expected restored thread in list, got %+v", list)
	}
	// Restoring an already-live thread should ErrNotFound (nothing to undo).
	if err := s.Restore(a.ID); err != ErrNotFound {
		t.Errorf("double-restore err = %v, want ErrNotFound", err)
	}
}
