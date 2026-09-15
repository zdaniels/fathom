package scheduler

import (
	"context"
	"github.com/zdaniels/fathom/internal/brandenv"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"os"
)

// Invoker runs a single scheduled prompt against the agent.
type Invoker func(ctx context.Context, prompt string) (string, error)

// Deliverer surfaces the result somewhere — stdout by default, a Slack post
// or email in future.
type Deliverer func(job Job, reply string)

// Scheduler polls the store on a ticker and runs due jobs through the agent.
type Scheduler struct {
	store     *Store
	invoke    Invoker
	deliver   Deliverer
	tickEvery time.Duration

	mu        sync.Mutex
	inFlight  map[string]struct{}
	stopCh    chan struct{}
	stoppedCh chan struct{}
}

// Options configures a new Scheduler.
type Options struct {
	Store     *Store
	Invoker   Invoker
	Deliverer Deliverer
	TickEvery time.Duration
}

// New builds a Scheduler. Default tick is 30s.
func New(opts Options) *Scheduler {
	tick := opts.TickEvery
	if tick == 0 {
		tick = 30 * time.Second
	}
	return &Scheduler{
		store:     opts.Store,
		invoke:    opts.Invoker,
		deliver:   opts.Deliverer,
		tickEvery: tick,
		inFlight:  make(map[string]struct{}),
	}
}

// Start begins the tick loop in the background. Idempotent — second call is
// a no-op.
func (s *Scheduler) Start() {
	if s.stopCh != nil {
		return
	}
	s.stopCh = make(chan struct{})
	s.stoppedCh = make(chan struct{})
	slog.Info("scheduler started", "tickEvery", s.tickEvery)
	go func() {
		defer close(s.stoppedCh)
		t := time.NewTicker(s.tickEvery)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := s.Tick(context.Background(), time.Now()); err != nil {
					slog.Error("scheduler tick failed", "err", err)
				}
			case <-s.stopCh:
				return
			}
		}
	}()
}

// Stop signals the loop to exit and waits for the current tick to finish.
func (s *Scheduler) Stop() {
	if s.stopCh == nil {
		return
	}
	close(s.stopCh)
	<-s.stoppedCh
	s.stopCh = nil
	s.stoppedCh = nil
	slog.Info("scheduler stopped")
}

// Tick runs one pass: fetch due jobs, fire any that aren't already in flight.
// Exposed so tests can step the scheduler deterministically.
func (s *Scheduler) Tick(ctx context.Context, now time.Time) error {
	due, err := s.store.Due(now)
	if err != nil {
		return err
	}
	for _, job := range due {
		s.mu.Lock()
		_, busy := s.inFlight[job.ID]
		s.mu.Unlock()
		if busy {
			slog.Debug("skipping job already in flight", "id", job.ID)
			continue
		}
		s.runJob(ctx, job, now)
	}
	return nil
}

// RunNow fires a specific job regardless of its schedule. Used by `fathom
// schedule run`.
func (s *Scheduler) RunNow(ctx context.Context, id string) (string, error) {
	job, err := s.store.Get(id)
	if err != nil {
		return "", err
	}
	s.runJob(ctx, job, time.Now())
	updated, err := s.store.Get(id)
	if err != nil {
		return "", err
	}
	return updated.LastResult, nil
}

func (s *Scheduler) runJob(ctx context.Context, job Job, runAt time.Time) {
	s.mu.Lock()
	s.inFlight[job.ID] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.inFlight, job.ID)
		s.mu.Unlock()
	}()

	slog.Info("running scheduled job", "id", job.ID, "cron", job.Cron, "skill", job.Skill)
	reply, err := s.invoke(ctx, SkillDirective(job.Skill, job.Prompt))
	if err != nil {
		reply = "error: " + err.Error()
		slog.Error("scheduled job failed", "id", job.ID, "err", err)
	}
	if err := s.store.MarkRun(job.ID, runAt, reply); err != nil {
		slog.Warn("markRun failed", "id", job.ID, "err", err)
	}
	if s.deliver != nil {
		s.deliver(job, reply)
	}
}

// ConsoleDeliverer prints each job's result to stdout, formatted for the
// gateway boot terminal.
func ConsoleDeliverer(job Job, reply string) {
	ts := time.Now().UTC().Format(time.RFC3339)
	first := "  [" + ts + "] job " + job.ID[:8] + " (" + job.Cron + ")"
	os.Stdout.WriteString("\n" + first + "\n")
	os.Stdout.WriteString("  prompt: " + job.Prompt + "\n")
	os.Stdout.WriteString("  reply:  " + reply + "\n\n")
}

// DefaultDBPath returns the SQLite store location. Precedence:
//
//  1. $FANTAZM_SCHEDULE_DB (explicit override)
//  2. ./data/schedule.db when run from inside a fathom project dir
//     (matches dataDir in fathom.config.yaml so per-project schedules
//     don't leak across projects)
//  3. ~/.fantazm/schedule.db otherwise
func DefaultDBPath() string {
	if v := brandenv.Get("FATHOM_SCHEDULE_DB"); v != "" {
		return v
	}
	if _, err := os.Stat("./data"); err == nil {
		return filepath.Join("data", "schedule.db")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".fantazm", "schedule.db")
}
