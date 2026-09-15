package agent

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// ----- Step 1: blackboard -----

func TestBlackboardPostReadDigest(t *testing.T) {
	b := newBlackboard(nil)
	b.setRound(1)
	b.post("main", "planner", "step one")
	b.post("", "coder", "wrote code") // empty channel → "main"
	b.setRound(2)
	b.post("review", "critic", "looks good")

	if got := len(b.read("")); got != 3 {
		t.Fatalf("read all = %d entries, want 3", got)
	}
	if got := len(b.read("main")); got != 2 {
		t.Errorf("read main = %d, want 2 (empty channel defaults to main)", got)
	}
	if got := b.read("review"); len(got) != 1 || got[0].Round != 2 || got[0].Author != "critic" {
		t.Errorf("review entry wrong: %+v", got)
	}
	dg := b.digest()
	for _, want := range []string{"channel: main", "channel: review", "planner", "critic"} {
		if !strings.Contains(dg, want) {
			t.Errorf("digest missing %q:\n%s", want, dg)
		}
	}
}

func TestBlackboardDoneAuthors(t *testing.T) {
	b := newBlackboard(nil)
	b.post("done", "a", "x")
	b.post("done", "a", "again") // same author counts once
	b.post("main", "b", "y")     // not done
	done := b.doneAuthors()
	if !done["a"] || done["b"] || len(done) != 1 {
		t.Errorf("doneAuthors = %v, want {a}", done)
	}
}

func TestSwarmBoardToolsPostAndRead(t *testing.T) {
	b := newBlackboard(nil)
	b.setRound(1)
	tools := swarmBoardTools(b, "planner")
	post, read := tools[0], tools[1]
	if post.Name != "swarm_post" || read.Name != "swarm_read" {
		t.Fatalf("tool names = %q,%q", post.Name, read.Name)
	}

	// Missing content → error.
	if _, err := post.Execute(context.Background(), map[string]interface{}{}, ToolContext{}); err == nil {
		t.Error("swarm_post with no content should error")
	}
	// Post is tagged with the bound author, not anything from params.
	if _, err := post.Execute(context.Background(),
		map[string]interface{}{"channel": "main", "content": "hello"}, ToolContext{}); err != nil {
		t.Fatalf("swarm_post: %v", err)
	}
	got := b.read("main")
	if len(got) != 1 || got[0].Author != "planner" || got[0].Content != "hello" {
		t.Errorf("posted entry = %+v, want author=planner content=hello", got)
	}
	// Read returns entries.
	out, err := read.Execute(context.Background(), map[string]interface{}{"channel": "main"}, ToolContext{})
	if err != nil {
		t.Fatalf("swarm_read: %v", err)
	}
	m := out.(map[string]interface{})
	if entries, ok := m["entries"].([]BoardEntry); !ok || len(entries) != 1 {
		t.Errorf("read entries = %v", m["entries"])
	}
}

// ----- Step 2: swarm tool guardrails (no model invoked) -----

func TestSwarmRejectsNestedLaunch(t *testing.T) {
	tool := NewSwarmTool(DelegateDeps{Router: testRouter(t)}, 0, 0)
	ctx := withDepth(context.Background(), 1) // a sub-agent trying to launch a swarm
	_, err := tool.Execute(ctx, map[string]interface{}{
		"goal":  "x",
		"roles": []interface{}{map[string]interface{}{"name": "a", "instructions": "i"}, map[string]interface{}{"name": "b", "instructions": "i"}},
	}, ToolContext{})
	if err == nil || !strings.Contains(err.Error(), "don't nest") {
		t.Fatalf("nested swarm: want nesting error, got %v", err)
	}
}

func TestSwarmValidation(t *testing.T) {
	tool := NewSwarmTool(DelegateDeps{Router: testRouter(t)}, 0, 0)
	twoRoles := []interface{}{
		map[string]interface{}{"name": "a", "instructions": "do a"},
		map[string]interface{}{"name": "b", "instructions": "do b"},
	}
	cases := []struct {
		name   string
		params map[string]interface{}
		want   string
	}{
		{"missing goal", map[string]interface{}{"roles": twoRoles}, "'goal' is required"},
		{"too few roles", map[string]interface{}{"goal": "g", "roles": []interface{}{map[string]interface{}{"name": "a", "instructions": "i"}}}, "at least 2 roles"},
		{"missing instructions", map[string]interface{}{"goal": "g", "roles": []interface{}{
			map[string]interface{}{"name": "a"}, map[string]interface{}{"name": "b", "instructions": "i"}}}, "missing 'instructions'"},
		{"duplicate name", map[string]interface{}{"goal": "g", "roles": []interface{}{
			map[string]interface{}{"name": "a", "instructions": "i"}, map[string]interface{}{"name": "a", "instructions": "i"}}}, "duplicate role"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := tool.Execute(context.Background(), c.params, ToolContext{})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestParseRolesCapsAndFields(t *testing.T) {
	// Over the max-roles cap.
	many := make([]interface{}, swarmMaxRoles+1)
	for i := range many {
		many[i] = map[string]interface{}{"name": string(rune('a' + i)), "instructions": "i"}
	}
	if _, err := parseRoles(many); err == nil || !strings.Contains(err.Error(), "too many roles") {
		t.Errorf("over cap: want too-many error, got %v", err)
	}
	// Fields parsed through.
	roles, err := parseRoles([]interface{}{
		map[string]interface{}{"name": "coder", "instructions": "write", "model": "hermes", "tools": []interface{}{"file-editor"}, "difficulty": "hard"},
		map[string]interface{}{"name": "critic", "instructions": "review"},
	})
	if err != nil {
		t.Fatalf("parseRoles: %v", err)
	}
	if roles[0].name != "coder" || roles[0].model != "hermes" || roles[0].difficulty != "hard" ||
		len(roles[0].tools) != 1 || roles[0].tools[0] != "file-editor" {
		t.Errorf("role[0] = %+v", roles[0])
	}
}

// ----- Step 2: coordinator round/termination loop (members short-circuit) -----

// TestSwarmCoordinatorRoundsAndTermination drives the full coordinator with
// BuildChildTools returning nil, so every member fails fast (no model call).
// We assert the round loop honours maxRounds, records member failures, and
// returns the structured result shape with the right termination reason.
func TestSwarmCoordinatorRoundsAndTermination(t *testing.T) {
	var buildCalls atomic.Int32 // members run concurrently → atomic
	deps := DelegateDeps{
		Router: testRouter(t),
		BuildChildTools: func([]string, bool) *ToolRegistry {
			buildCalls.Add(1)
			return nil // members + synth fail fast, no model invoked
		},
	}
	tool := NewSwarmTool(deps, 2 /*maxRounds*/, 0)
	out, err := tool.Execute(context.Background(), map[string]interface{}{
		"goal": "build a thing",
		"roles": []interface{}{
			map[string]interface{}{"name": "planner", "instructions": "plan it"},
			map[string]interface{}{"name": "coder", "instructions": "code it"},
		},
	}, ToolContext{})
	if err != nil {
		t.Fatalf("swarm execute: %v", err)
	}
	res := out.(map[string]interface{})
	if res["rounds"].(int) != 2 {
		t.Errorf("rounds = %v, want 2", res["rounds"])
	}
	if res["terminated"].(string) != "max_rounds" {
		t.Errorf("terminated = %v, want max_rounds", res["terminated"])
	}
	// 2 rounds × 2 members = 4 member builds + 1 synth build = 5.
	if got := buildCalls.Load(); got != 5 {
		t.Errorf("BuildChildTools calls = %d, want 5 (4 members + 1 synth)", got)
	}
	// Member failures are recorded on the transcript's "errors" channel.
	transcript := res["transcript"].([]BoardEntry)
	errs := 0
	for _, e := range transcript {
		if e.Channel == "errors" {
			errs++
		}
	}
	if errs != 4 {
		t.Errorf("error entries = %d, want 4 (one per failed member-round)", errs)
	}
}

// maxRounds is clamped to the hard ceiling regardless of the configured default.
func TestSwarmMaxRoundsCeiling(t *testing.T) {
	deps := DelegateDeps{
		Router:          testRouter(t),
		BuildChildTools: func([]string, bool) *ToolRegistry { return nil },
	}
	tool := NewSwarmTool(deps, 999 /*absurd default*/, 0)
	out, _ := tool.Execute(context.Background(), map[string]interface{}{
		"goal": "g",
		"roles": []interface{}{
			map[string]interface{}{"name": "a", "instructions": "i"},
			map[string]interface{}{"name": "b", "instructions": "i"},
		},
		"maxRounds": float64(50),
	}, ToolContext{})
	if got := out.(map[string]interface{})["rounds"].(int); got != swarmMaxRoundsCap {
		t.Errorf("rounds = %d, want capped at %d", got, swarmMaxRoundsCap)
	}
}

// ----- Follow-up 1: persistent role memory -----

func TestAppendNoteGrowthAndCap(t *testing.T) {
	// Empty reply leaves prior untouched.
	if got := appendNote("prior", 2, "   "); got != "prior" {
		t.Errorf("empty reply changed notes: %q", got)
	}
	// Grows with round-tagged entries.
	n := appendNote("", 1, "first")
	n = appendNote(n, 2, "second")
	if !strings.Contains(n, "Round 1: first") || !strings.Contains(n, "Round 2: second") {
		t.Errorf("notes missing rounds: %q", n)
	}
	// Caps the tail and keeps the most recent content.
	big := strings.Repeat("x", swarmMemoryCap*2)
	capped := appendNote(big, 9, "latest-content")
	if max := swarmMemoryCap + len("…"); len(capped) > max { // leading ellipsis marker
		t.Errorf("capped length = %d, want <= %d", len(capped), max)
	}
	if !strings.Contains(capped, "latest-content") {
		t.Error("cap dropped the most recent content")
	}
}

// ----- Follow-up 2: debate/review presets -----

func TestPresetRoles(t *testing.T) {
	deb, err := presetRoles("debate", "solve X")
	if err != nil || len(deb) != 3 {
		t.Fatalf("debate preset = %d roles, %v; want 3", len(deb), err)
	}
	if deb[2].name != "judge" {
		t.Errorf("debate last role = %q, want judge", deb[2].name)
	}
	rev, err := presetRoles("REVIEW", "write Y") // case-insensitive
	if err != nil || len(rev) != 2 || rev[0].name != "author" || rev[1].name != "critic" {
		t.Fatalf("review preset = %+v, %v", rev, err)
	}
	if _, err := presetRoles("nonsense", "g"); err == nil || !strings.Contains(err.Error(), "unknown preset") {
		t.Errorf("unknown preset: want error, got %v", err)
	}
}

func TestSwarmPresetExpandsAndRuns(t *testing.T) {
	deps := DelegateDeps{
		Router:          testRouter(t),
		BuildChildTools: func([]string, bool) *ToolRegistry { return nil },
	}
	tool := NewSwarmTool(deps, 1, 0)
	// preset, no explicit roles → should expand and run.
	out, err := tool.Execute(context.Background(), map[string]interface{}{
		"goal": "design a logo", "preset": "review",
	}, ToolContext{})
	if err != nil {
		t.Fatalf("preset swarm: %v", err)
	}
	if out.(map[string]interface{})["rounds"].(int) != 1 {
		t.Errorf("preset swarm rounds = %v, want 1", out.(map[string]interface{})["rounds"])
	}
	// Neither roles nor preset → clear error.
	if _, err := tool.Execute(context.Background(), map[string]interface{}{"goal": "g"}, ToolContext{}); err == nil ||
		!strings.Contains(err.Error(), "either 'roles'") {
		t.Errorf("no roles/preset: want guidance error, got %v", err)
	}
}

// ----- Follow-up 3: observability -----

func TestSwarmObserverReceivesEvents(t *testing.T) {
	deps := DelegateDeps{
		Router:          testRouter(t),
		BuildChildTools: func([]string, bool) *ToolRegistry { return nil }, // members fail → "errors" posts
	}
	var mu sync.Mutex
	kinds := map[string]int{}
	ctx := WithSwarmObserver(context.Background(), func(ev SwarmEvent) {
		mu.Lock()
		kinds[ev.Kind]++
		mu.Unlock()
	})
	tool := NewSwarmTool(deps, 2, 0)
	_, err := tool.Execute(ctx, map[string]interface{}{
		"goal": "g",
		"roles": []interface{}{
			map[string]interface{}{"name": "a", "instructions": "i"},
			map[string]interface{}{"name": "b", "instructions": "i"},
		},
	}, ToolContext{})
	if err != nil {
		t.Fatalf("swarm: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{"round_start", "round_end", "synthesis", "complete", "post"} {
		if kinds[want] == 0 {
			t.Errorf("observer never saw %q event (saw: %v)", want, kinds)
		}
	}
	if kinds["round_start"] != 2 {
		t.Errorf("round_start count = %d, want 2", kinds["round_start"])
	}
}
