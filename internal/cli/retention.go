package cli

import (
	"context"
	"fmt"
	"github.com/zdaniels/fathom/internal/security"
	"github.com/zdaniels/fathom/internal/threads"
	"github.com/zdaniels/fathom/pkg/types"
	"log/slog"
	"time"
)

func runRetention(ctx context.Context, cfg types.Config, audit *security.AuditLogger, store *threads.Store) {
	auditDays, trashDays := 90, 30
	if cfg.Retention != nil {
		auditDays, trashDays = cfg.Retention.AuditDays, cfg.Retention.DeletedThreadDays
	}
	sweep := func() {
		now := time.Now()
		if audit != nil && auditDays > 0 {
			if _, err := audit.PruneBefore(now.Add(-time.Duration(auditDays) * 24 * time.Hour)); err != nil {
				slog.Error("audit retention failed", "err", err)
			}
		}
		if store != nil && trashDays > 0 {
			if _, err := store.PurgeDeletedBefore(now.Add(-time.Duration(trashDays) * 24 * time.Hour)); err != nil {
				slog.Error("thread retention failed", "err", err)
			}
		}
	}
	sweep()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

func validateRetention(cfg types.Config) error {
	if cfg.Retention != nil {
		for _, n := range []int{cfg.Retention.AuditDays, cfg.Retention.DeletedThreadDays} {
			if n < 0 || n > 36500 {
				return fmt.Errorf("retention days must be between 0 (disabled) and 36500")
			}
		}
	}
	return nil
}
