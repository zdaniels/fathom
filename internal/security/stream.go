package security

import (
	"context"
	"unicode/utf8"

	"github.com/zdaniels/fathom/internal/streamtext"
)

// FilterStream keeps a 64-byte lookbehind (issued canaries are 35 bytes), so
// a canary split across provider deltas cannot leak through the live UI before
// the agent loop performs its full-response check. Private reasoning is never
// supplied by provider adapters. Flush only after a successful provider call.
func (c *CanarySystem) FilterStream(ctx context.Context) (context.Context, func()) {
	if c == nil || !streamtext.Enabled(ctx) {
		return ctx, func() {}
	}
	pending := ""
	blocked := false
	filtered := streamtext.With(ctx, func(delta string) {
		if blocked {
			return
		}
		pending += delta
		if c.Check(pending) != nil {
			blocked = true
			pending = ""
			return
		}
		n := len(pending) - 64
		if n > 0 {
			for n > 0 && !utf8.RuneStart(pending[n]) {
				n--
			}
			streamtext.Emit(ctx, pending[:n])
			pending = pending[n:]
		}
	})
	return filtered, func() {
		if !blocked {
			streamtext.Emit(ctx, pending)
		}
		pending = ""
	}
}
