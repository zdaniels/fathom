package agent

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zdaniels/fathom/internal/agent/llm"
	"github.com/zdaniels/fathom/internal/security"
	"github.com/zdaniels/fathom/pkg/types"
)

// stubProvider returns the canned responses queued in `replies`, in order.
// One response per Chat call.
type stubProvider struct {
	replies []llm.Response
	calls   int
	mu      atomic.Int32
}

func (s *stubProvider) Chat(ctx context.Context, _ []llm.Message, _ []llm.ToolDef) (llm.Response, error) {
	i := s.mu.Add(1) - 1
	if int(i) >= len(s.replies) {
		return llm.Response{}, errors.New("no more stub replies")
	}
	s.calls = int(i) + 1
	return s.replies[i], nil
}

func mkMesh(t *testing.T) *security.Mesh {
	t.Helper()
	return security.NewMesh(types.PolicyConfig{
		Defaults: types.PermissionSet{Network: "allow", Filesystem: "read-write", Shell: "allow", Secrets: "accessible"},
	}, security.MeshOptions{})
}

func mkSession() types.Session {
	now := time.Now()
	return types.Session{
		ID:        "s1",
		UserID:    "u1",
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
		Permissions: types.PermissionSet{
			Network: "allow", Filesystem: "read-write", Shell: "allow", Secrets: "accessible",
		},
	}
}

func TestLoopSimpleTextResponse(t *testing.T) {
	mesh := mkMesh(t)
	tools := NewToolRegistry(mesh.Policy, mesh.Audit, mesh.Canary)
	stub := &stubProvider{replies: []llm.Response{
		{Content: "hi from agent", FinishReason: llm.FinishStop},
	}}
	loop := NewLoop(LoopOptions{Provider: stub, Tools: tools, Mesh: mesh})

	reply, err := loop.Process(context.Background(),
		types.ChannelMessage{Text: "hello", Timestamp: time.Now()},
		mkSession())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if reply != "hi from agent" {
		t.Errorf("reply = %q, want 'hi from agent'", reply)
	}
}

func TestLoopRunsToolThenContinues(t *testing.T) {
	mesh := mkMesh(t)
	tools := NewToolRegistry(mesh.Policy, mesh.Audit, mesh.Canary)
	tools.Register(ToolDefinition{
		Name:        "echo",
		Description: "echo back",
		Parameters:  map[string]interface{}{"type": "object"},
		Execute: func(ctx context.Context, p map[string]interface{}, _ ToolContext) (interface{}, error) {
			return p["text"], nil
		},
	})

	stub := &stubProvider{replies: []llm.Response{
		{
			Content: "let me call echo",
			ToolCalls: []llm.ToolCallRequest{{
				ID:        "call-1",
				Name:      "echo",
				Arguments: map[string]interface{}{"text": "hello"},
			}},
			FinishReason: llm.FinishToolCalls,
		},
		{Content: "final answer: hello", FinishReason: llm.FinishStop},
	}}
	loop := NewLoop(LoopOptions{Provider: stub, Tools: tools, Mesh: mesh})

	reply, err := loop.Process(context.Background(),
		types.ChannelMessage{Text: "echo hello"},
		mkSession())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if reply != "final answer: hello" {
		t.Errorf("reply = %q, want 'final answer: hello'", reply)
	}
	if stub.calls != 2 {
		t.Errorf("LLM call count = %d, want 2 (tool-call then completion)", stub.calls)
	}
}

func TestLoopHitsMaxIterations(t *testing.T) {
	mesh := mkMesh(t)
	tools := NewToolRegistry(mesh.Policy, mesh.Audit, mesh.Canary)
	tools.Register(ToolDefinition{
		Name:       "loop",
		Parameters: map[string]interface{}{"type": "object"},
		Execute: func(ctx context.Context, _ map[string]interface{}, _ ToolContext) (interface{}, error) {
			return "x", nil
		},
	})

	// Build N+2 stub replies that all request the tool, never finish.
	var replies []llm.Response
	for i := 0; i < 12; i++ {
		replies = append(replies, llm.Response{
			Content: "again",
			ToolCalls: []llm.ToolCallRequest{
				{ID: "c", Name: "loop", Arguments: map[string]interface{}{}},
			},
			FinishReason: llm.FinishToolCalls,
		})
	}
	stub := &stubProvider{replies: replies}
	loop := NewLoop(LoopOptions{Provider: stub, Tools: tools, Mesh: mesh, MaxIterations: 3})

	reply, err := loop.Process(context.Background(),
		types.ChannelMessage{Text: "loop me"}, mkSession())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if reply == "" || stub.calls != 3 {
		t.Errorf("hit iteration cap: replied=%q calls=%d (want 3)", reply, stub.calls)
	}
}

// fakeSpan records every method call so tests can assert what got emitted.
type fakeSpan struct {
	t      *fakeTracer
	name   string
	attrs  map[string]string
	errMsg string
	ended  bool
	parent *fakeSpan
}

func (s *fakeSpan) SetAttr(k string, v any) Span {
	if s.attrs == nil {
		s.attrs = map[string]string{}
	}
	s.attrs[k] = fmt.Sprintf("%v", v)
	return s
}
func (s *fakeSpan) SetError(msg string) Span { s.errMsg = msg; return s }
func (s *fakeSpan) End()                     { s.ended = true }
func (s *fakeSpan) TraceID() string          { return "fake" }

type fakeTracer struct{ spans []*fakeSpan }

func (t *fakeTracer) StartSpan(_ context.Context, name string) Span {
	s := &fakeSpan{t: t, name: name}
	t.spans = append(t.spans, s)
	return s
}
func (t *fakeTracer) StartChild(parent Span, name string) Span {
	p, _ := parent.(*fakeSpan)
	s := &fakeSpan{t: t, name: name, parent: p}
	t.spans = append(t.spans, s)
	return s
}

func TestLoopEmitsTracerSpans(t *testing.T) {
	mesh := mkMesh(t)
	tools := NewToolRegistry(mesh.Policy, mesh.Audit, mesh.Canary)
	tools.Register(ToolDefinition{
		Name:       "echo",
		Parameters: map[string]interface{}{"type": "object"},
		Execute: func(ctx context.Context, p map[string]interface{}, _ ToolContext) (interface{}, error) {
			return p["text"], nil
		},
	})

	stub := &stubProvider{replies: []llm.Response{
		{
			Content: "calling echo",
			ToolCalls: []llm.ToolCallRequest{{
				ID: "c1", Name: "echo", Arguments: map[string]interface{}{"text": "ping"},
			}},
			FinishReason: llm.FinishToolCalls,
		},
		{Content: "done", FinishReason: llm.FinishStop},
	}}

	tracer := &fakeTracer{}
	loop := NewLoop(LoopOptions{Provider: stub, Tools: tools, Mesh: mesh, Tracer: tracer})
	if _, err := loop.Process(context.Background(),
		types.ChannelMessage{Text: "go"}, mkSession()); err != nil {
		t.Fatalf("Process: %v", err)
	}

	// Expected: 1 root + 2 iterations + 2 llm.chat + 1 tool span = 6
	want := []string{"agent.process", "agent.iteration", "llm.chat", "tool.echo", "agent.iteration", "llm.chat"}
	if len(tracer.spans) != len(want) {
		t.Fatalf("span count = %d, want %d (names: %v)", len(tracer.spans), len(want), spanNames(tracer.spans))
	}
	for i, n := range want {
		if tracer.spans[i].name != n {
			t.Errorf("span[%d].name = %q, want %q", i, tracer.spans[i].name, n)
		}
		if !tracer.spans[i].ended {
			t.Errorf("span[%d] (%s) never ended", i, tracer.spans[i].name)
		}
	}
	if tracer.spans[0].attrs["outcome"] != "ok" {
		t.Errorf("root outcome = %q, want 'ok'", tracer.spans[0].attrs["outcome"])
	}
}

func TestLoopNoopTracerWhenNil(t *testing.T) {
	// nil tracer must not panic — Loop should swap in its noopTracer.
	mesh := mkMesh(t)
	tools := NewToolRegistry(mesh.Policy, mesh.Audit, mesh.Canary)
	stub := &stubProvider{replies: []llm.Response{
		{Content: "hi", FinishReason: llm.FinishStop},
	}}
	loop := NewLoop(LoopOptions{Provider: stub, Tools: tools, Mesh: mesh, Tracer: nil})
	if _, err := loop.Process(context.Background(),
		types.ChannelMessage{Text: "x"}, mkSession()); err != nil {
		t.Fatalf("Process: %v", err)
	}
}

func spanNames(spans []*fakeSpan) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.name
	}
	return out
}

func TestToolRegistryBindsSkillNameToGetSecret(t *testing.T) {
	// Validate that ToolContext.GetSecret is bound to the calling tool's
	// SkillName when the resolver is set — the regression target is
	// "secretGetter ignored skill scope" from the TS review.
	mesh := mkMesh(t)
	tools := NewToolRegistry(mesh.Policy, mesh.Audit, mesh.Canary)

	var seenSkill string
	tools.SetSecretResolver(func(name, skill string) (string, error) {
		seenSkill = skill
		return "value-for-" + name, nil
	})
	tools.Register(ToolDefinition{
		Name:       "gh_lookup",
		SkillName:  "github",
		Parameters: map[string]interface{}{"type": "object"},
		Execute: func(ctx context.Context, _ map[string]interface{}, tctx ToolContext) (interface{}, error) {
			return tctx.GetSecret("GITHUB_TOKEN")
		},
	})

	res := tools.Execute(context.Background(),
		llm.ToolCallRequest{ID: "1", Name: "gh_lookup", Arguments: map[string]interface{}{}},
		mkSession())
	if !res.Success {
		t.Fatalf("Execute failed: %v", res.Error)
	}
	if seenSkill != "github" {
		t.Errorf("resolver got skill=%q, want 'github'", seenSkill)
	}
}
