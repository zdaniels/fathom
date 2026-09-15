package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/zdaniels/fathom/internal/streamtext"
)

type claudeStreamWriter struct {
	ctx     context.Context
	pending []byte
	result  []byte
}

func (w *claudeStreamWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.pending = append(w.pending, p...)
	if len(w.pending) > 1<<20 {
		return 0, errors.New("Claude stream event exceeded 1 MB")
	}
	for {
		idx := bytes.IndexByte(w.pending, '\n')
		if idx < 0 {
			break
		}
		line := w.pending[:idx]
		var e struct {
			Type  string
			Event struct {
				Type  string
				Delta struct{ Type, Text string }
			}
		}
		if err := json.Unmarshal(line, &e); err != nil {
			return 0, err
		}
		if e.Type == "result" {
			w.result = append(w.result[:0], line...)
		} else if e.Type == "stream_event" && e.Event.Type == "content_block_delta" && e.Event.Delta.Type == "text_delta" {
			streamtext.Emit(w.ctx, e.Event.Delta.Text)
		}
		w.pending = w.pending[idx+1:]
	}
	return n, nil
}
