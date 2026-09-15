package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/zdaniels/fathom/internal/agent/llm"
	"github.com/zdaniels/fathom/pkg/types"
)

func TestDepthContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	if got := depthFromCtx(ctx); got != 0 {
		t.Fatalf("empty ctx depth = %d, want 0", got)
	}
	ctx = withDepth(ctx, 3)
	if got := depthFromCtx(ctx); got != 3 {
		t.Fatalf("depth after withDepth(3) = %d, want 3", got)
	}
}

func TestAsStringSlice(t *testing.T) {
	cases := []struct {
		in   interface{}
		want []string
	}{
		{[]interface{}{"a", "b"}, []string{"a", "b"}},
		{[]interface{}{"a", "", "  ", "c"}, []string{"a", "c"}}, // drops empty/blank
		{[]interface{}{"a", 1, "b"}, []string{"a", "b"}},        // drops non-strings
		{[]string{"x", "y"}, []string{"x", "y"}},
		{"not-a-slice", nil},
		{nil, nil},
	}
	for i, c := range cases {
		got := asStringSlice(c.in)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("case %d: asStringSlice(%v) = %v, want %v", i, c.in, got, c.want)
		}
	}
}

func TestContainsString(t *testing.T) {
	hs := []string{"opus", "qwen-large", "hermes"}
	if !containsString(hs, "qwen-large") {
		t.Error("expected qwen-large present")
	}
	if containsString(hs, "qwen") {
		t.Error("qwen should not match qwen-large (exact only)")
	}
	if containsString(nil, "x") {
		t.Error("nil haystack should contain nothing")
	}
}

// testRouter builds a real llm.Router with a single local (ollama) model
// so Names()/validation work without any API key or network call. The
// model is never actually invoked in these guardrail tests.
func testRouter(t *testing.T) *llm.Router {
	t.Helper()
	r, err := llm.NewRouter(types.LLMConfig{
		Default: "local",
		Models: map[string]types.LLMModelConfig{
			"local": {Provider: "ollama", Model: "test-model"},
		},
	}, func(string) (string, error) { return "", nil })
	if err != nil {
		t.Fatalf("build test router: %v", err)
	}
	return r
}

func TestDelegateDepthCap(t *testing.T) {
	buildCalled := false
	tool := NewDelegateTool(DelegateDeps{
		Router:   testRouter(t),
		MaxDepth: 2,
		BuildChildTools: func([]string, bool) *ToolRegistry {
			buildCalled = true // must NOT be reached at the cap
			return nil
		},
	})

	// At depth == MaxDepth, Execute must refuse before building any child.
	ctx := withDepth(context.Background(), 2)
	_, err := tool.Execute(ctx, map[string]interface{}{"task": "do a thing"}, ToolContext{})
	if err == nil || !strings.Contains(err.Error(), "depth limit") {
		t.Fatalf("expected depth-limit error, got %v", err)
	}
	if buildCalled {
		t.Error("BuildChildTools should not run when the depth cap is hit")
	}
}

func testRouterWith(t *testing.T, names ...string) *llm.Router {
	t.Helper()
	models := map[string]types.LLMModelConfig{}
	for _, n := range names {
		models[n] = types.LLMModelConfig{Provider: "ollama", Model: n}
	}
	r, err := llm.NewRouter(types.LLMConfig{Default: names[0], Models: models},
		func(string) (string, error) { return "", nil })
	if err != nil {
		t.Fatalf("build router: %v", err)
	}
	return r
}

func TestResolveModelHardPreference(t *testing.T) {
	// Only qwen-large registered (opus/sonnet keys "absent") → hard auto
	// falls to the first preference that's actually registered.
	deps := DelegateDeps{
		Router:              testRouterWith(t, "qwen-large", "hermes"),
		HardModelPreference: []string{"opus", "sonnet", "qwen-large"},
		PickModel:           func(context.Context, string) string { return "hermes" },
	}
	got, err := deps.resolveModel(context.Background(), childSpec{task: "x", difficulty: "hard"})
	if err != nil || got != "qwen-large" {
		t.Fatalf("hard auto = %q, %v; want qwen-large", got, err)
	}

	// opus registered → hard auto prefers it.
	deps.Router = testRouterWith(t, "opus", "qwen-large", "hermes")
	got, _ = deps.resolveModel(context.Background(), childSpec{task: "x", difficulty: "hard"})
	if got != "opus" {
		t.Fatalf("hard auto with opus present = %q; want opus", got)
	}

	// Explicit model overrides difficulty entirely.
	got, err = deps.resolveModel(context.Background(), childSpec{task: "x", model: "hermes", difficulty: "hard"})
	if err != nil || got != "hermes" {
		t.Fatalf("explicit model = %q, %v; want hermes", got, err)
	}

	// Normal difficulty → classifier/PickModel.
	got, _ = deps.resolveModel(context.Background(), childSpec{task: "x"})
	if got != "hermes" {
		t.Fatalf("normal auto = %q; want hermes (from PickModel)", got)
	}
}

func TestDelegateParallelGuards(t *testing.T) {
	built := false
	tools := NewSubAgentTools(DelegateDeps{
		Router:   testRouter(t),
		MaxDepth: 2,
		BuildChildTools: func([]string, bool) *ToolRegistry {
			built = true
			return NewToolRegistry(nil, nil, nil)
		},
	})
	// tools[1] is delegate_parallel.
	par := tools[1]
	if par.Name != "delegate_parallel" {
		t.Fatalf("expected delegate_parallel, got %q", par.Name)
	}

	// Empty tasks → error, no child built.
	_, err := par.Execute(context.Background(), map[string]interface{}{"tasks": []interface{}{}}, ToolContext{})
	if err == nil || !strings.Contains(err.Error(), "non-empty array") {
		t.Fatalf("empty tasks: want non-empty-array error, got %v", err)
	}
	// Over the cap → error.
	many := make([]interface{}, maxParallelTasks+1)
	for i := range many {
		many[i] = map[string]interface{}{"task": "x"}
	}
	_, err = par.Execute(context.Background(), map[string]interface{}{"tasks": many}, ToolContext{})
	if err == nil || !strings.Contains(err.Error(), "too many tasks") {
		t.Fatalf("over cap: want too-many error, got %v", err)
	}
	// Depth cap → error before building anything.
	_, err = par.Execute(withDepth(context.Background(), 2),
		map[string]interface{}{"tasks": []interface{}{map[string]interface{}{"task": "x"}}}, ToolContext{})
	if err == nil || !strings.Contains(err.Error(), "depth limit") {
		t.Fatalf("depth cap: want depth-limit error, got %v", err)
	}
	if built {
		t.Error("no child registry should be built on guard failures")
	}
}

func TestDelegateMissingTask(t *testing.T) {
	tool := NewDelegateTool(DelegateDeps{Router: testRouter(t)})
	_, err := tool.Execute(context.Background(), map[string]interface{}{"task": "   "}, ToolContext{})
	if err == nil || !strings.Contains(err.Error(), "'task' is required") {
		t.Fatalf("expected missing-task error, got %v", err)
	}
}

func TestDelegateUnknownModel(t *testing.T) {
	tool := NewDelegateTool(DelegateDeps{
		Router:          testRouter(t),
		BuildChildTools: func([]string, bool) *ToolRegistry { return NewToolRegistry(nil, nil, nil) },
	})
	_, err := tool.Execute(context.Background(),
		map[string]interface{}{"task": "x", "model": "gpt-9-ultra"}, ToolContext{})
	if err == nil || !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("expected unknown-model error, got %v", err)
	}
}

// TestDelegateReadOnlyDefault asserts the tool-set decision: omitting
// `tools` selects the read-only default (readOnly=true); naming tools
// selects them (readOnly=false). We capture the args via a spy and
// return a nil registry so Execute fails fast right after — before any
// model call — which is fine: we only care about the selection inputs.
func TestDelegateToolSelection(t *testing.T) {
	var gotReadOnly bool
	var gotNames []string
	spy := func(names []string, readOnly bool) *ToolRegistry {
		gotNames, gotReadOnly = names, readOnly
		return nil // forces a clean early return after selection
	}
	tool := NewDelegateTool(DelegateDeps{Router: testRouter(t), BuildChildTools: spy})

	// No tools → read-only default.
	_, _ = tool.Execute(context.Background(), map[string]interface{}{"task": "x"}, ToolContext{})
	if !gotReadOnly || len(gotNames) != 0 {
		t.Errorf("omitted tools: readOnly=%v names=%v, want readOnly=true names=[]", gotReadOnly, gotNames)
	}

	// Explicit tools → not read-only, names passed through.
	_, _ = tool.Execute(context.Background(),
		map[string]interface{}{"task": "x", "tools": []interface{}{"file-editor", "shell"}}, ToolContext{})
	if gotReadOnly || strings.Join(gotNames, ",") != "file-editor,shell" {
		t.Errorf("explicit tools: readOnly=%v names=%v, want readOnly=false names=[file-editor shell]", gotReadOnly, gotNames)
	}
}
