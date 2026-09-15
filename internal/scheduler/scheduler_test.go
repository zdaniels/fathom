package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSchedulerInFlightGuard(t *testing.T) {
	// If a tick is still running and the next tick fires, the same job
	// must NOT run twice in parallel.
	s := openTempStore(t)
	_, _ = s.Add("* * * * *", "slow", time.Date(2026, 5, 22, 18, 0, 0, 0, time.UTC))

	var concurrent int32
	var max int32
	var mu sync.Mutex
	sched := New(Options{
		Store: s,
		Invoker: func(ctx context.Context, prompt string) (string, error) {
			n := atomic.AddInt32(&concurrent, 1)
			mu.Lock()
			if n > max {
				max = n
			}
			mu.Unlock()
			time.Sleep(50 * time.Millisecond)
			atomic.AddInt32(&concurrent, -1)
			return "ok", nil
		},
		Deliverer: func(_ Job, _ string) {},
	})
	// Two ticks in parallel against the same future "now" → both due.
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = sched.Tick(context.Background(), time.Now().Add(time.Hour))
		}()
	}
	wg.Wait()
	if max > 1 {
		t.Errorf("max concurrent runs = %d, want 1 (in-flight guard failed)", max)
	}
}

func TestSchedulerRunNowExecutesJob(t *testing.T) {
	s := openTempStore(t)
	job, _ := s.Add("@daily", "do thing", time.Now())
	var ran int32
	sched := New(Options{
		Store: s,
		Invoker: func(ctx context.Context, prompt string) (string, error) {
			atomic.AddInt32(&ran, 1)
			return "reply: " + prompt, nil
		},
		Deliverer: func(_ Job, _ string) {},
	})
	reply, err := sched.RunNow(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if atomic.LoadInt32(&ran) != 1 {
		t.Errorf("invoker ran %d times, want 1", ran)
	}
	if reply != "reply: do thing" {
		t.Errorf("RunNow reply = %q, want %q", reply, "reply: do thing")
	}
}

func TestSchedulerTickRunsDueJobs(t *testing.T) {
	s := openTempStore(t)
	now := time.Date(2026, 5, 22, 18, 0, 0, 0, time.UTC)
	_, _ = s.Add("* * * * *", "minutely", now)
	var ran int32
	sched := New(Options{
		Store: s,
		Invoker: func(ctx context.Context, prompt string) (string, error) {
			atomic.AddInt32(&ran, 1)
			return "ok", nil
		},
		Deliverer: func(_ Job, _ string) {},
	})
	// Step well past the next_run_at.
	if err := sched.Tick(context.Background(), now.Add(10*time.Minute)); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if atomic.LoadInt32(&ran) != 1 {
		t.Errorf("Tick ran %d invocations, want 1", ran)
	}
}
