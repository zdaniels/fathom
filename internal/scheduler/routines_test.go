package scheduler

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRoutineCRUD(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()

	existed, err := store.SaveRoutine("briefing", "summarize my PRs", "github", now)
	if err != nil || existed {
		t.Fatalf("create: existed=%v err=%v", existed, err)
	}
	r, err := store.GetRoutine("briefing")
	if err != nil || r.Prompt != "summarize my PRs" || r.Skill != "github" {
		t.Fatalf("get: %+v err=%v", r, err)
	}
	created := r.CreatedAt

	// update (upsert): prompt/skill change, created_at preserved
	existed, err = store.SaveRoutine("briefing", "summarize my PRs and issues", "", now.Add(time.Hour))
	if err != nil || !existed {
		t.Fatalf("update: existed=%v err=%v", existed, err)
	}
	r, _ = store.GetRoutine("briefing")
	if r.Prompt != "summarize my PRs and issues" || r.Skill != "" {
		t.Fatalf("after update: %+v", r)
	}
	if !r.CreatedAt.Equal(created) {
		t.Errorf("created_at changed on update: %v -> %v", created, r.CreatedAt)
	}

	_, _ = store.SaveRoutine("inbox", "check email", "gmail", now)
	rs, err := store.ListRoutines()
	if err != nil || len(rs) != 2 || rs[0].Name != "briefing" || rs[1].Name != "inbox" {
		t.Fatalf("list ordered by name: %+v err=%v", rs, err)
	}

	ok, err := store.DeleteRoutine("inbox")
	if err != nil || !ok {
		t.Fatalf("delete: ok=%v err=%v", ok, err)
	}
	if _, err := store.GetRoutine("inbox"); err == nil {
		t.Fatal("expected inbox to be gone")
	}
}

func TestRoutineValidation(t *testing.T) {
	store, _ := OpenStore(filepath.Join(t.TempDir(), "r.db"))
	defer store.Close()
	now := time.Now()
	for _, c := range []struct{ name, prompt string }{
		{"bad name", "p"}, // space in name
		{"", "p"},         // empty name
		{"ok", "   "},     // empty prompt
	} {
		if _, err := store.SaveRoutine(c.name, c.prompt, "", now); err == nil {
			t.Errorf("SaveRoutine(%q,%q) want error", c.name, c.prompt)
		}
	}
}

func TestSkillDirective(t *testing.T) {
	if got := SkillDirective("", "hello"); got != "hello" {
		t.Errorf("empty skill should pass through, got %q", got)
	}
	got := SkillDirective("gmail", "summarize unread")
	if !strings.Contains(got, "gmail") || !strings.Contains(got, "summarize unread") {
		t.Errorf("directive missing parts: %q", got)
	}
}

func TestRunJobAppliesSkillDirective(t *testing.T) {
	store, _ := OpenStore(filepath.Join(t.TempDir(), "j.db"))
	defer store.Close()
	job, _ := store.AddWithSkill("* * * * *", "summarize unread", "gmail", time.Now())

	var gotPrompt string
	s := New(Options{
		Store: store,
		Invoker: func(ctx context.Context, prompt string) (string, error) {
			gotPrompt = prompt
			return "ok", nil
		},
	})
	if _, err := s.RunNow(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotPrompt, "gmail") || !strings.Contains(gotPrompt, "summarize unread") {
		t.Errorf("scheduled invoker should receive the skill directive, got %q", gotPrompt)
	}

	// A skill-less job passes the prompt through unchanged.
	plain, _ := store.Add("* * * * *", "just do it", time.Now())
	if _, err := s.RunNow(context.Background(), plain.ID); err != nil {
		t.Fatal(err)
	}
	if gotPrompt != "just do it" {
		t.Errorf("skill-less job should pass prompt unchanged, got %q", gotPrompt)
	}
}

func TestAddWithSkill(t *testing.T) {
	store, _ := OpenStore(filepath.Join(t.TempDir(), "j.db"))
	defer store.Close()
	job, err := store.AddWithSkill("0 9 * * *", "do it", "gmail", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.Get(job.ID); err != nil || got.Skill != "gmail" {
		t.Fatalf("roundtrip skill: %+v err=%v", got, err)
	}
	j2, _ := store.Add("0 9 * * *", "plain", time.Now())
	if g2, _ := store.Get(j2.ID); g2.Skill != "" {
		t.Fatalf("Add should leave skill empty, got %q", g2.Skill)
	}
}
