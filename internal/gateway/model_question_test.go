package gateway

import (
	"strings"
	"testing"

	"github.com/zdaniels/fathom/internal/threads"
	"github.com/zdaniels/fathom/pkg/types"
)

// In takeover mode the model-question interceptor must report the takeover
// target — not a local router model — and must NOT advertise /use or /models
// (which don't apply when the router is bypassed).
func TestModelQuestionUnderTakeover(t *testing.T) {
	g := New(types.Config{
		Takeover: &types.TakeoverConfig{Enabled: true, Model: "sonnet"},
	})
	reply, handled := g.maybeHandleModelQuestion(threads.Thread{}, "what model are you using?")
	if !handled {
		t.Fatal("model question should be handled")
	}
	if !strings.Contains(reply, "takeover") || !strings.Contains(reply, "claude:sonnet") {
		t.Errorf("takeover reply should name the takeover target, got: %q", reply)
	}
	if strings.Contains(reply, "/use") || strings.Contains(reply, "/models") {
		t.Errorf("takeover reply should not advertise /use or /models, got: %q", reply)
	}
}

// A per-thread model pin is irrelevant under takeover — takeover still wins.
func TestModelQuestionTakeoverBeatsThreadPin(t *testing.T) {
	g := New(types.Config{Takeover: &types.TakeoverConfig{Enabled: true}})
	reply, handled := g.maybeHandleModelQuestion(threads.Thread{Model: "qwen-large"}, "what model")
	if !handled || strings.Contains(reply, "qwen-large") {
		t.Errorf("takeover should override the thread pin, got handled=%v reply=%q", handled, reply)
	}
}

// With takeover off, behavior is unchanged: a thread pin is reported and the
// /use hint is present.
func TestModelQuestionNoTakeoverUsesThreadPin(t *testing.T) {
	g := New(types.Config{}) // no takeover
	reply, handled := g.maybeHandleModelQuestion(threads.Thread{Model: "qwen"}, "what model are you")
	if !handled {
		t.Fatal("should be handled")
	}
	if !strings.Contains(reply, "qwen") || !strings.Contains(reply, "/use") {
		t.Errorf("non-takeover reply should report the pin + /use hint, got: %q", reply)
	}
}

// Non-model questions are left alone in every mode.
func TestModelQuestionIgnoresOtherText(t *testing.T) {
	g := New(types.Config{Takeover: &types.TakeoverConfig{Enabled: true}})
	if _, handled := g.maybeHandleModelQuestion(threads.Thread{}, "what is the capital of germany"); handled {
		t.Error("non-model question should not be intercepted")
	}
}
