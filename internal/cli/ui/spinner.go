package ui

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// Spinner is a soft three-dot animation. Cobalt color, no label noise.
// Lives on its own line and clears itself on Stop().
type Spinner struct {
	out     io.Writer
	stop    chan struct{}
	wg      sync.WaitGroup
	started time.Time
}

// Three-dot pulsing patterns. Lighter than braille — less drawing the eye.
var spinnerFrames = []string{"·  ", "·· ", "···", " ··", "  ·", "   "}

// StartSpinner kicks off a goroutine that animates in cobalt until Stop().
// On a non-TTY it's a single quiet "thinking" line instead.
func StartSpinner(out io.Writer, _ string) *Spinner {
	s := &Spinner{out: out, stop: make(chan struct{}), started: time.Now()}
	if !enabled {
		fmt.Fprintln(out, "  "+Mute("thinking"))
		return s
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(120 * time.Millisecond)
		defer t.Stop()
		i := 0
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
				secs := int(time.Since(s.started).Seconds())
				suffix := ""
				if secs > 2 {
					suffix = fmt.Sprintf("  %s", Mute(fmt.Sprintf("(%ds)", secs)))
				}
				fmt.Fprintf(out, "\r  %s%s", Accent(spinnerFrames[i%len(spinnerFrames)]), suffix)
				i++
			}
		}
	}()
	return s
}

// Stop halts and clears the spinner line.
func (s *Spinner) Stop() {
	if s.stop == nil {
		return
	}
	close(s.stop)
	s.wg.Wait()
	s.stop = nil
	if enabled {
		fmt.Fprint(s.out, "\r\x1b[2K")
	}
}
