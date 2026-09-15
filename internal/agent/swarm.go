package agent

// Agent swarms: the `swarm` tool runs several role-agents against a shared
// blackboard over multiple coordination rounds, then synthesises their work
// into one answer. It builds on the sub-agent machinery (DelegateDeps,
// child loops, the model router) and adds the things that turn isolated
// delegation into a collaborating swarm:
//
//	Blackboard      — shared, append-only state members read + write via the
//	                  swarm_post / swarm_read tools, so they see each other.
//	Coordinator     — a bounded round loop with explicit termination
//	                  (all-done, max-rounds, token budget, wall-clock) + a
//	                  final synthesis pass.
//	Role memory     — each role carries a private scratchpad across rounds,
//	                  separate from the shared board, so it remembers its own
//	                  line of work.
//	Presets         — `preset: "debate" | "review"` expands to a ready-made
//	                  set of roles so common patterns are one call.
//	Observability   — a SwarmObserver callback (carried via context, mirrors
//	                  DelegateNotifier) fires on every round + board post, so
//	                  the gateway can stream swarm progress to clients.
//
// Synchronous rounds: every member reads the same start-of-round snapshot
// and posts concurrently, so a round has no intra-round ordering
// dependence — deterministic and easy to reason about.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/zdaniels/fathom/pkg/types"
)

const (
	swarmDefaultRounds = 3
	swarmMaxRoundsCap  = 6 // hard ceiling regardless of config/params
	swarmMinRoles      = 2
	swarmMaxRoles      = 6
	swarmTimeout       = 10 * time.Minute
	swarmDigestPerChan = 12   // max entries per channel injected into prompts
	swarmMemoryCap     = 4000 // max chars of a role's private memory carried forward
)

const swarmMemberPersona = `You are one agent in a collaborating swarm working toward a shared goal. ` +
	`Read what other members have already contributed on the blackboard, then add YOUR part — don't repeat work that's done. ` +
	`Share findings with swarm_post (channel "main" by default); pull a channel with swarm_read. ` +
	`When you believe the whole goal is fully met, post to the "done" channel (or include [DONE] in your reply). ` +
	`Be concise and concrete. Make reasonable assumptions rather than asking questions.`

const swarmSynthPersona = `You are the synthesiser for a swarm. Read the entire blackboard and produce the single best ` +
	`final answer to the goal, reconciling and merging the members' contributions. Be complete but concise; do not mention the swarm process.`

// ----- Observability -----

// SwarmEvent is one observable moment in a swarm run. Kind is one of
// round_start, post, round_end, synthesis, complete.
type SwarmEvent struct {
	Kind    string `json:"kind"`
	Round   int    `json:"round,omitempty"`
	Author  string `json:"author,omitempty"`
	Channel string `json:"channel,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// SwarmObserver is called as a swarm progresses. The gateway wires one (via
// the context) to stream "swarm: planner posted on main" events to clients;
// the CLI path leaves it nil. Must tolerate concurrent calls — board posts
// happen from parallel member goroutines.
type SwarmObserver func(SwarmEvent)

type swarmObserverKey struct{}

// WithSwarmObserver returns a context carrying fn, surfaced to clients.
func WithSwarmObserver(ctx context.Context, fn SwarmObserver) context.Context {
	return context.WithValue(ctx, swarmObserverKey{}, fn)
}

func swarmObserverFrom(ctx context.Context) SwarmObserver {
	if fn, ok := ctx.Value(swarmObserverKey{}).(SwarmObserver); ok {
		return fn
	}
	return nil
}

// emit fires the observer (if any) and always logs at debug, so there is
// some observability even when no client is wired.
func emit(obs SwarmObserver, ev SwarmEvent) {
	slog.Debug("swarm event", "kind", ev.Kind, "round", ev.Round, "author", ev.Author, "channel", ev.Channel)
	if obs != nil {
		obs(ev)
	}
}

// ----- Blackboard -----

// BoardEntry is one append-only contribution on the shared blackboard.
type BoardEntry struct {
	Author  string    `json:"author"`
	Channel string    `json:"channel"`
	Content string    `json:"content"`
	Round   int       `json:"round"`
	At      time.Time `json:"at"`
}

// Blackboard is the swarm's shared memory: an append-only log, mutex-guarded
// so concurrent members can post during a round without tearing the slice.
type Blackboard struct {
	mu      sync.Mutex
	entries []BoardEntry
	round   int
	obs     SwarmObserver
}

func newBlackboard(obs SwarmObserver) *Blackboard { return &Blackboard{obs: obs} }

func (b *Blackboard) setRound(r int) {
	b.mu.Lock()
	b.round = r
	b.mu.Unlock()
}

func (b *Blackboard) post(channel, author, content string) {
	channel = strings.TrimSpace(channel)
	if channel == "" {
		channel = "main"
	}
	b.mu.Lock()
	round := b.round
	b.entries = append(b.entries, BoardEntry{
		Author: author, Channel: channel, Content: content, Round: round, At: time.Now().UTC(),
	})
	obs := b.obs
	b.mu.Unlock()
	// Fire the observer outside the lock — the callback may be slow.
	emit(obs, SwarmEvent{Kind: "post", Round: round, Author: author, Channel: channel, Detail: preview(content, 120)})
}

func (b *Blackboard) read(channel string) []BoardEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]BoardEntry, 0, len(b.entries))
	for _, e := range b.entries {
		if channel == "" || e.Channel == channel {
			out = append(out, e)
		}
	}
	return out
}

// doneAuthors returns the set of members who have signalled completion by
// posting to the "done" channel.
func (b *Blackboard) doneAuthors() map[string]bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := map[string]bool{}
	for _, e := range b.entries {
		if e.Channel == "done" {
			m[e.Author] = true
		}
	}
	return m
}

// digest renders a compact, readable snapshot grouped by channel for
// injection into member prompts (small local models are far more reliable
// reading state from the prompt than from a tool call). Capped per channel.
func (b *Blackboard) digest() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.entries) == 0 {
		return "(blackboard is empty)"
	}
	byChan := map[string][]BoardEntry{}
	order := []string{}
	for _, e := range b.entries {
		if _, seen := byChan[e.Channel]; !seen {
			order = append(order, e.Channel)
		}
		byChan[e.Channel] = append(byChan[e.Channel], e)
	}
	var sb strings.Builder
	for _, ch := range order {
		entries := byChan[ch]
		if len(entries) > swarmDigestPerChan {
			entries = entries[len(entries)-swarmDigestPerChan:]
		}
		fmt.Fprintf(&sb, "## channel: %s\n", ch)
		for _, e := range entries {
			fmt.Fprintf(&sb, "- [r%d %s] %s\n", e.Round, e.Author, preview(e.Content, 600))
		}
	}
	return sb.String()
}

// swarmBoardTools builds the swarm_post / swarm_read tools bound to one
// member: posts are auto-tagged with the member's name + the current round,
// so authorship can't be spoofed by the model.
func swarmBoardTools(board *Blackboard, author string) []ToolDefinition {
	post := ToolDefinition{
		Name: "swarm_post",
		Description: "Share a contribution on the shared swarm blackboard so other members can see it. " +
			"Use channel \"done\" when you believe the whole goal is fully met.",
		SkillName: "swarm",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"channel": map[string]interface{}{"type": "string", "description": "Channel to post on (default \"main\"; use \"done\" to signal completion)."},
				"content": map[string]interface{}{"type": "string", "description": "The contribution to share."},
			},
			"required": []string{"content"},
		},
		Execute: func(_ context.Context, p map[string]interface{}, _ ToolContext) (interface{}, error) {
			content := strings.TrimSpace(asString(p["content"]))
			if content == "" {
				return nil, fmt.Errorf("swarm_post: 'content' is required")
			}
			board.post(asString(p["channel"]), author, content)
			return map[string]interface{}{"ok": true}, nil
		},
	}
	read := ToolDefinition{
		Name:        "swarm_read",
		Description: "Read contributions from the shared swarm blackboard. Omit channel to read everything.",
		SkillName:   "swarm",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"channel": map[string]interface{}{"type": "string", "description": "Channel to read (omit for all)."},
			},
		},
		Execute: func(_ context.Context, p map[string]interface{}, _ ToolContext) (interface{}, error) {
			return map[string]interface{}{"entries": board.read(strings.TrimSpace(asString(p["channel"])))}, nil
		},
	}
	return []ToolDefinition{post, read}
}

// ----- Coordinator -----

// swarmRole is one persistent member identity in a swarm.
type swarmRole struct {
	name         string
	model        string
	tools        []string
	difficulty   string
	instructions string
}

type swarmUsage struct{ prompt, completion int }

// runSwarmMember runs one member's turn for a round: builds its tool set
// (its own builtins + the board tools), injects the goal + role + its private
// notes + the current board snapshot, runs a short focused loop, records the
// reply on the "main" channel, and returns its updated private notes.
func runSwarmMember(ctx context.Context, deps DelegateDeps, depth int, tctx ToolContext, board *Blackboard, role swarmRole, goal, priorNotes string, round, maxRounds int) (swarmUsage, string, error) {
	model, err := deps.resolveModel(ctx, childSpec{task: goal + " " + role.instructions, model: role.model, difficulty: role.difficulty})
	if err != nil {
		return swarmUsage{}, priorNotes, err
	}
	tools := deps.BuildChildTools(role.tools, len(role.tools) == 0)
	if tools == nil {
		return swarmUsage{}, priorNotes, fmt.Errorf("could not build swarm member tools")
	}
	for _, t := range swarmBoardTools(board, role.name) {
		tools.Register(t)
	}

	notesBlock := ""
	if strings.TrimSpace(priorNotes) != "" {
		notesBlock = "\n\nYour private working notes from earlier rounds (only you see these):\n" + priorNotes + "\n"
	}
	prompt := fmt.Sprintf(
		"You are \"%s\", a member of a swarm.\n\nGOAL:\n%s\n\nYOUR ROLE:\n%s%s\n\n"+
			"This is round %d of up to %d. The shared blackboard so far:\n\n%s\n\n"+
			"Read the above, then contribute your part. Share results with swarm_post; signal completion on the \"done\" channel when the GOAL is fully met.",
		role.name, goal, role.instructions, notesBlock, round, maxRounds, board.digest())

	if notify := delegateNotifierFrom(ctx); notify != nil {
		notify(model, fmt.Sprintf("swarm:%s r%d", role.name, round))
	}

	loop := NewLoop(LoopOptions{
		Router:        deps.Router,
		Tools:         tools,
		Mesh:          deps.Mesh,
		Persona:       swarmMemberPersona,
		MaxIterations: subChildIterations,
	})
	cctx, cancel := context.WithTimeout(withDepth(ctx, depth+1), subChildTimeout)
	defer cancel()
	res, err := loop.ProcessV2(cctx, types.ChannelMessage{
		ChannelType: "swarm", ChannelID: tctx.SessionID, SenderID: tctx.UserID,
		Text: prompt, Timestamp: time.Now().UTC(),
	}, types.Session{ID: tctx.SessionID, UserID: tctx.UserID, Permissions: tctx.Permissions}, model)
	if err != nil {
		return swarmUsage{}, priorNotes, err
	}
	reply := strings.TrimSpace(res.Reply)
	if reply != "" {
		// Record the round summary so the board has every member's
		// contribution even when a small model ignores swarm_post.
		board.post("main", role.name, reply)
		if strings.Contains(strings.ToUpper(reply), "[DONE]") {
			board.post("done", role.name, "signalled via reply")
		}
	}
	return swarmUsage{prompt: res.Usage.PromptTokens, completion: res.Usage.CompletionTokens},
		appendNote(priorNotes, round, reply), nil
}

// appendNote grows a role's private memory with this round's reply, keeping
// the tail bounded so prompts don't balloon over many rounds.
func appendNote(prior string, round int, reply string) string {
	if strings.TrimSpace(reply) == "" {
		return prior
	}
	note := fmt.Sprintf("Round %d: %s", round, reply)
	combined := note
	if prior != "" {
		combined = prior + "\n" + note
	}
	if len(combined) > swarmMemoryCap {
		combined = "…" + combined[len(combined)-swarmMemoryCap:]
	}
	return combined
}

// runSwarm is the coordinator: seed the goal, run synchronous rounds of all
// members (bounded concurrency) carrying per-role memory, check termination
// after each, then a final synthesis pass. Returns a structured result.
func runSwarm(ctx context.Context, deps DelegateDeps, tctx ToolContext, goal string, roles []swarmRole, maxRounds, tokenBudget int) map[string]interface{} {
	obs := swarmObserverFrom(ctx)
	board := newBlackboard(obs)
	board.post("goal", "coordinator", goal)
	depth := depthFromCtx(ctx)

	roleMemory := make([]string, len(roles)) // index i is private to role i
	var totalPrompt, totalCompletion int
	roundsRun := 0
	reason := "max_rounds"

	for round := 1; round <= maxRounds; round++ {
		select {
		case <-ctx.Done():
			reason = "timeout"
		default:
		}
		if reason == "timeout" {
			break
		}
		board.setRound(round)
		roundsRun = round
		emit(obs, SwarmEvent{Kind: "round_start", Round: round, Detail: fmt.Sprintf("round %d/%d", round, maxRounds)})

		// Run all members for this round with bounded concurrency. Each
		// goroutine touches only its own slice index, so no locking needed.
		usages := make([]swarmUsage, len(roles))
		sem := make(chan struct{}, maxParallelWorkers)
		var wg sync.WaitGroup
		for i := range roles {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				u, notes, err := runSwarmMember(ctx, deps, depth, tctx, board, roles[i], goal, roleMemory[i], round, maxRounds)
				if err != nil {
					board.post("errors", roles[i].name, "member failed: "+err.Error())
					return
				}
				usages[i] = u
				roleMemory[i] = notes
			}(i)
		}
		wg.Wait()

		for _, u := range usages {
			totalPrompt += u.prompt
			totalCompletion += u.completion
		}
		emit(obs, SwarmEvent{Kind: "round_end", Round: round})

		// Termination checks.
		if done := board.doneAuthors(); len(done) >= len(roles) {
			reason = "all_done"
			break
		}
		if tokenBudget > 0 && totalPrompt+totalCompletion >= tokenBudget {
			reason = "budget"
			break
		}
	}

	emit(obs, SwarmEvent{Kind: "synthesis", Round: roundsRun})
	final := synthesise(ctx, deps, depth, tctx, board, goal)
	emit(obs, SwarmEvent{Kind: "complete", Round: roundsRun, Detail: reason})
	slog.Info("swarm completed",
		"roles", len(roles), "rounds", roundsRun, "reason", reason,
		"prompt_tokens", totalPrompt, "completion_tokens", totalCompletion)

	return map[string]interface{}{
		"final":             final,
		"rounds":            roundsRun,
		"terminated":        reason,
		"prompt_tokens":     totalPrompt,
		"completion_tokens": totalCompletion,
		"transcript":        board.read(""),
	}
}

// synthesise runs one final child loop that reads the whole board and
// produces the consolidated answer. Model auto-picks (prefers strong on
// "hard"), with read-only tools — it only reads + answers.
func synthesise(ctx context.Context, deps DelegateDeps, depth int, tctx ToolContext, board *Blackboard, goal string) string {
	model, err := deps.resolveModel(ctx, childSpec{task: goal, difficulty: "hard"})
	if err != nil {
		model = ""
	}
	tools := deps.BuildChildTools(nil, true)
	if tools == nil {
		return "swarm synthesis unavailable (no tool registry)"
	}
	loop := NewLoop(LoopOptions{
		Router: deps.Router, Tools: tools, Mesh: deps.Mesh,
		Persona: swarmSynthPersona, MaxIterations: subChildIterations,
	})
	cctx, cancel := context.WithTimeout(withDepth(ctx, depth+1), subChildTimeout)
	defer cancel()
	prompt := fmt.Sprintf("GOAL:\n%s\n\nThe swarm's blackboard:\n\n%s\n\nProduce the final answer to the GOAL.", goal, board.digest())
	res, err := loop.ProcessV2(cctx, types.ChannelMessage{
		ChannelType: "swarm", ChannelID: tctx.SessionID, SenderID: tctx.UserID,
		Text: prompt, Timestamp: time.Now().UTC(),
	}, types.Session{ID: tctx.SessionID, UserID: tctx.UserID, Permissions: tctx.Permissions}, model)
	if err != nil {
		return "swarm synthesis failed: " + err.Error()
	}
	return res.Reply
}

// NewSwarmTool returns the `swarm` tool. defaultRounds/tokenBudget come from
// config; both are clamped by the engine. Only the top-level agent (depth 0)
// can launch a swarm — members run one level down without the swarm tool, so
// swarms can't nest.
func NewSwarmTool(deps DelegateDeps, defaultRounds, tokenBudget int) ToolDefinition {
	if defaultRounds <= 0 {
		defaultRounds = swarmDefaultRounds
	}
	return ToolDefinition{
		Name: "swarm",
		Description: "Run a small team of role-agents collaboratively on a shared blackboard over several rounds, " +
			"then synthesise their work into one answer. Use for open-ended or multi-perspective work (design + critique, " +
			"research + draft + review) where several specialists iterating together beats a single agent. " +
			"Provide either 2-6 explicit `roles`, or a `preset` (\"debate\" or \"review\") that supplies them. " +
			"For independent one-shot tasks use `delegate_parallel` instead.",
		SkillName:  "swarm",
		Parameters: swarmParamsSchema(),
		Execute: func(ctx context.Context, params map[string]interface{}, tctx ToolContext) (interface{}, error) {
			if depthFromCtx(ctx) > 0 {
				return nil, fmt.Errorf("swarm can only be launched by the top-level agent (swarms don't nest)")
			}
			goal := strings.TrimSpace(asString(params["goal"]))
			if goal == "" {
				return nil, fmt.Errorf("swarm: 'goal' is required")
			}
			roles, err := resolveRoles(params, goal)
			if err != nil {
				return nil, err
			}
			maxRounds := defaultRounds
			if v, ok := params["maxRounds"].(float64); ok && v > 0 {
				maxRounds = int(v)
			}
			if maxRounds > swarmMaxRoundsCap {
				maxRounds = swarmMaxRoundsCap
			}
			sctx, cancel := context.WithTimeout(ctx, swarmTimeout)
			defer cancel()
			return runSwarm(sctx, deps, tctx, goal, roles, maxRounds, tokenBudget), nil
		},
	}
}

// resolveRoles picks explicit roles when given, else expands a preset.
func resolveRoles(params map[string]interface{}, goal string) ([]swarmRole, error) {
	if raw, ok := params["roles"].([]interface{}); ok && len(raw) > 0 {
		return parseRoles(raw)
	}
	if preset := strings.TrimSpace(asString(params["preset"])); preset != "" {
		return presetRoles(preset, goal)
	}
	return nil, fmt.Errorf("swarm: provide either 'roles' (2-6) or a 'preset' (debate, review)")
}

// presetRoles expands a named collaboration pattern into roles. Roles use
// auto model-pick + the safe read-only tool set by default.
func presetRoles(preset, goal string) ([]swarmRole, error) {
	switch strings.ToLower(preset) {
	case "debate":
		return []swarmRole{
			{name: "proposer_a", instructions: "Propose a concrete solution to the goal and argue for it. Engage with the other proposer's points each round."},
			{name: "proposer_b", instructions: "Propose a DIFFERENT solution to the goal and argue for it. Challenge proposer_a's reasoning where it's weak."},
			{name: "judge", instructions: "Weigh both proposals impartially, identify the strongest elements of each, and steer toward the best combined answer. Declare done when a clear best answer has emerged."},
		}, nil
	case "review":
		return []swarmRole{
			{name: "author", instructions: "Produce and iteratively improve a complete draft answer to the goal, incorporating the critic's feedback each round."},
			{name: "critic", instructions: "Critique the author's latest draft: find errors, gaps, and weak spots, and give specific, actionable fixes. Signal done when the draft is solid."},
		}, nil
	default:
		return nil, fmt.Errorf("swarm: unknown preset %q (valid: debate, review)", preset)
	}
}

func parseRoles(raw []interface{}) ([]swarmRole, error) {
	if len(raw) < swarmMinRoles {
		return nil, fmt.Errorf("swarm: need at least %d roles (got %d) — use `delegate` for a single agent", swarmMinRoles, len(raw))
	}
	if len(raw) > swarmMaxRoles {
		return nil, fmt.Errorf("swarm: too many roles (%d); max is %d", len(raw), swarmMaxRoles)
	}
	roles := make([]swarmRole, 0, len(raw))
	seen := map[string]bool{}
	for i, rr := range raw {
		m, ok := rr.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("swarm: roles[%d] is not an object", i)
		}
		name := strings.TrimSpace(asString(m["name"]))
		if name == "" {
			return nil, fmt.Errorf("swarm: roles[%d] missing 'name'", i)
		}
		if seen[name] {
			return nil, fmt.Errorf("swarm: duplicate role name %q", name)
		}
		seen[name] = true
		instr := strings.TrimSpace(asString(m["instructions"]))
		if instr == "" {
			return nil, fmt.Errorf("swarm: role %q missing 'instructions'", name)
		}
		roles = append(roles, swarmRole{
			name:         name,
			model:        asString(m["model"]),
			tools:        asStringSlice(m["tools"]),
			difficulty:   asString(m["difficulty"]),
			instructions: instr,
		})
	}
	return roles, nil
}

func swarmParamsSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"goal": map[string]interface{}{
				"type":        "string",
				"description": "The shared objective for the whole swarm to achieve.",
			},
			"preset": map[string]interface{}{
				"type":        "string",
				"enum":        []string{"debate", "review"},
				"description": "Ready-made role set: \"debate\" (two proposers + a judge) or \"review\" (author + critic). Used when `roles` is omitted.",
			},
			"roles": map[string]interface{}{
				"type":        "array",
				"description": "2-6 role-agents that collaborate. Give each a distinct, complementary remit. Overrides `preset` when both are given.",
				"items": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name":         map[string]interface{}{"type": "string", "description": "Short role name, e.g. \"planner\", \"coder\", \"critic\"."},
						"instructions": map[string]interface{}{"type": "string", "description": "What this role should focus on within the goal."},
						"model":        map[string]interface{}{"type": "string", "description": "Model for this role (see /models). Omit/\"auto\" to let the router pick."},
						"difficulty":   map[string]interface{}{"type": "string", "enum": []string{"normal", "hard"}, "description": "\"hard\" prefers a strong model when auto-picking."},
						"tools":        map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Builtin tools this role may use. Omit for a safe read-only set."},
					},
					"required": []string{"name", "instructions"},
				},
			},
			"maxRounds": map[string]interface{}{
				"type":        "number",
				"description": "Max coordination rounds (capped by the engine). Omit for the configured default.",
			},
		},
		"required": []string{"goal"},
	}
}
