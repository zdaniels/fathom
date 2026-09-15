package scheduler

import (
	"path/filepath"
	"testing"
	"time"
)

func openTempStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sched.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStoreAddListGet(t *testing.T) {
	s := openTempStore(t)
	job, err := s.Add("@minutely", "say hi", time.Date(2026, 5, 22, 18, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if job.Cron != "* * * * *" {
		t.Errorf("expanded cron = %q, want '* * * * *'", job.Cron)
	}
	if job.NextRunAt.IsZero() {
		t.Error("Add did not set NextRunAt")
	}

	list, err := s.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("List: got %d, err=%v", len(list), err)
	}
	got, err := s.Get(job.ID)
	if err != nil || got.ID != job.ID {
		t.Errorf("Get: %v %s", err, got.ID)
	}
}

func TestStoreAddRejectsBadCron(t *testing.T) {
	s := openTempStore(t)
	if _, err := s.Add("foo bar baz", "x", time.Now()); err == nil {
		t.Error("Add should reject malformed cron")
	}
}

func TestStoreDelete(t *testing.T) {
	s := openTempStore(t)
	job, _ := s.Add("@daily", "x", time.Now())
	ok, err := s.Delete(job.ID)
	if err != nil || !ok {
		t.Fatalf("Delete: ok=%v err=%v", ok, err)
	}
	if _, err := s.Get(job.ID); err == nil {
		t.Error("Get after Delete should error")
	}
}

func TestStoreDueReturnsOnlyDue(t *testing.T) {
	s := openTempStore(t)
	now := time.Date(2026, 5, 22, 18, 0, 0, 0, time.UTC)
	// A every-minute job's next run is 18:01.
	_, _ = s.Add("* * * * *", "minutely", now)
	// A daily-at-00:00 job whose next run is tomorrow 00:00.
	_, _ = s.Add("@daily", "daily", now)

	// At 18:00:30, nothing is due yet.
	due, err := s.Due(now.Add(30 * time.Second))
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(due) != 0 {
		t.Errorf("Due at 18:00:30 = %d, want 0", len(due))
	}
	// At 18:01:30, the minutely job is due; the daily one is not.
	due, _ = s.Due(now.Add(90 * time.Second))
	if len(due) != 1 || due[0].Prompt != "minutely" {
		t.Errorf("Due at 18:01:30 = %+v, want [minutely]", due)
	}
}

func TestMarkRunAnchorsAtNowNotRunAt(t *testing.T) {
	// Regression for the catch-up loop bug: if MarkRun anchored at runAt,
	// a slow invoker would write a next_run_at in the past, and the next
	// tick would re-fire immediately. We anchor at time.Now() so the next
	// slot is always in the future.
	s := openTempStore(t)
	createdAt := time.Date(2026, 5, 22, 18, 0, 0, 0, time.UTC)
	job, _ := s.Add("* * * * *", "slow", createdAt)
	// Simulate a slow invoker: runAt was 5 minutes ago.
	runAt := createdAt
	if err := s.MarkRun(job.ID, runAt, "ok"); err != nil {
		t.Fatalf("MarkRun: %v", err)
	}
	updated, _ := s.Get(job.ID)
	if !updated.NextRunAt.After(time.Now()) {
		t.Errorf("MarkRun must anchor NextRunAt in the future (got %v, now=%v)",
			updated.NextRunAt, time.Now())
	}
}

func TestMarkRunParksImpossibleCronFarFuture(t *testing.T) {
	// Build a job whose cron is parseable but has no future occurrence —
	// we have to backdoor it because Add() rejects impossible crons.
	s := openTempStore(t)
	job, _ := s.Add("* * * * *", "x", time.Now())
	// Now poison the row with an impossible cron expression.
	_, err := s.db.Exec(`UPDATE jobs SET cron = ? WHERE id = ?`, "0 0 30 2 *", job.ID)
	if err != nil {
		t.Fatalf("update poison: %v", err)
	}
	if err := s.MarkRun(job.ID, time.Now(), "ok"); err != nil {
		t.Fatalf("MarkRun: %v", err)
	}
	updated, _ := s.Get(job.ID)
	// Must be in the far future — not in the past (which would cause an
	// infinite re-fire loop).
	if !updated.NextRunAt.After(time.Now().Add(180 * 24 * time.Hour)) {
		t.Errorf("MarkRun should park impossible cron far future, got %v", updated.NextRunAt)
	}
}
