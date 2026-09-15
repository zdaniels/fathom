package agent

// Sub-agents: the `delegate` (and `delegate_parallel`) tools let the main
// agent hand scoped tasks to child agent loops running on a chosen model
// (local or cloud). Each child runs to completion with its own context +
// a narrowed tool set, then returns its final text to the parent.
//
// Why the closures: package `agent` can't import internal/builtin (that
// package imports us) or internal/agent/router cleanly, so the two
// host-specific capabilities — building a child tool registry from
// builtin tools, and classifier-based model selection — are injected by
// internal/agentfactory, which sits above both.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/zdaniels/fathom/internal/agent/llm"
	"github.com/zdaniels/fathom/internal/security"
	"github.com/zdaniels/fathom/pkg/types"
)

// subAgentDepthKey carries the current delegation depth through context.
// Top-level (the user-facing agent) is depth 0; each delegate hop adds 1.
type subAgentDepthKey struct{}

func depthFromCtx(ctx context.Context) int {
	if v, ok := ctx.Value(subAgentDepthKey{}).(int); ok {
		return v
	}
	return 0
}

func withDepth(ctx context.Context, d int) context.Context {
	return context.WithValue(ctx, subAgentDepthKey{}, d)
}

// delegateNotifierKey carries an optional per-request callback the host
// (e.g. the gateway) sets to surface "delegating to <model>" events to
// connected clients. nil when unset — the CLI path doesn't wire one.
type delegateNotifierKey struct{}

// DelegateNotifier is called just before a child loop starts, with the
// resolved model name and a short preview of the delegated task.
type DelegateNotifier func(model, taskPreview string)

// WithDelegateNotifier returns a context carrying fn. The gateway wraps
// the per-request context with this so the delegate tool can publish a
// thread event without the agent package importing the gateway.
func WithDelegateNotifier(ctx context.Context, fn DelegateNotifier) context.Context {
	return context.WithValue(ctx, delegateNotifierKey{}, fn)
}

func delegateNotifierFrom(ctx context.Context) DelegateNotifier {
	if fn, ok := ctx.Value(delegateNotifierKey{}).(DelegateNotifier); ok {
		return fn
	}
	return nil
}

const (
	defaultSubAgentMaxDepth = 2
	subChildIterations      = 8 // a delegated task should be focused
	subChildTimeout         = 3 * time.Minute
	maxParallelTasks        = 8 // cap fan-out per delegate_parallel call
	maxParallelWorkers      = 4 // bound concurrent child loops
)

// subAgentPersona is prepended for child loops. Deliberately terse and
// task-focused: the child exists to complete one delegated job and
// return a self-contained result, not to hold a conversation.
const subAgentPersona = `You are a focused sub-agent spawned to complete a single delegated task. ` +
	`Do exactly what the task asks using the tools you have, then return a concise, self-contained result ` +
	`the calling agent can use directly. Do not ask clarifying questions — make reasonable assumptions and state them. ` +
	`Do not start unrelated work.`

// DelegateDeps are the host capabilities the sub-agent tools need,
// injected by internal/agentfactory (which can reach builtin tools +
// the classifier without an import cycle).
type DelegateDeps struct {
	// Router is the shared multi-model registry; child loops resolve their
	// model through it, so any configured local or cloud model is reachable.
	Router *llm.Router
	// Mesh is the security mesh; child tool calls go through the same policy
	// engine + audit log as the parent.
	Mesh *security.Mesh
	// MaxDepth caps nesting. 0 → defaultSubAgentMaxDepth.
	MaxDepth int
	// PickModel chooses a model name for "auto"/empty selections (typically
	// the classifier → profile → model). Returning "" means "router default".
	// May be nil (then auto → router default).
	PickModel func(ctx context.Context, task string) string
	// HardModelPreference is the ordered list of model names to prefer when
	// the caller passes difficulty="hard" and lets the model auto-pick. The
	// first name that's actually registered (i.e. its key is present) wins;
	// if none are registered we fall back to PickModel. Typically
	// ["opus","sonnet","qwen-large"].
	HardModelPreference []string
	// BuildChildTools builds a wired tool registry for a child. When
	// readOnly is true, skillNames is ignored and a curated read-only set is
	// used; otherwise the named builtin skills are included. The returned
	// registry already has the mesh + secret resolver + memory tools wired.
	BuildChildTools func(skillNames []string, readOnly bool) *ToolRegistry
}

// childSpec is one delegated task, parsed from tool params.
type childSpec struct {
	task       string
	model      string
	tools      []string
	difficulty string
	context    string
}

// resolveModel turns a spec's model + difficulty into a concrete model
// name (or "" for the router default). Explicit names are validated.
func (deps DelegateDeps) resolveModel(ctx context.Context, spec childSpec) (string, error) {
	m := strings.TrimSpace(spec.model)
	if m != "" && !strings.EqualFold(m, "auto") {
		if !containsString(deps.Router.Names(), m) {
			return "", fmt.Errorf("unknown model %q (configured: %s)", m, strings.Join(deps.Router.Names(), ", "))
		}
		return m, nil
	}
	// Auto. For hard tasks, prefer a strong model that's actually available
	// (registered == key present), else fall through to the classifier.
	if strings.EqualFold(spec.difficulty, "hard") {
		for _, pref := range deps.HardModelPreference {
			if containsString(deps.Router.Names(), pref) {
				return pref, nil
			}
		}
	}
	if deps.PickModel != nil {
		return deps.PickModel(ctx, spec.task), nil
	}
	return "", nil
}

// runChild builds + runs one child loop to completion and returns a
// structured result. self is the `delegate` tool, re-registered into the
// child registry only while another level of nesting stays under the cap.
func runChild(ctx context.Context, deps DelegateDeps, maxDepth int, self ToolDefinition, depth int, tctx ToolContext, spec childSpec) (map[string]interface{}, error) {
	model, err := deps.resolveModel(ctx, spec)
	if err != nil {
		return nil, err
	}
	childTools := deps.BuildChildTools(spec.tools, len(spec.tools) == 0)
	if childTools == nil {
		return nil, fmt.Errorf("could not build sub-agent tools")
	}
	if depth+1 < maxDepth {
		childTools.Register(self)
	}

	full := spec.task
	if extra := strings.TrimSpace(spec.context); extra != "" {
		full = spec.task + "\n\nContext from the calling agent:\n" + extra
	}

	if notify := delegateNotifierFrom(ctx); notify != nil {
		notify(model, preview(spec.task, 80))
	}

	child := NewLoop(LoopOptions{
		Router:        deps.Router,
		Tools:         childTools,
		Mesh:          deps.Mesh,
		Persona:       subAgentPersona,
		MaxIterations: subChildIterations,
	})
	cctx, cancel := context.WithTimeout(withDepth(ctx, depth+1), subChildTimeout)
	defer cancel()
	childMsg := types.ChannelMessage{
		ChannelType: "subagent",
		ChannelID:   tctx.SessionID,
		SenderID:    tctx.UserID,
		Text:        full,
		Timestamp:   time.Now().UTC(),
	}
	childSession := types.Session{ID: tctx.SessionID, UserID: tctx.UserID, Permissions: tctx.Permissions}
	res, err := child.ProcessV2(cctx, childMsg, childSession, model)
	if err != nil {
		return nil, fmt.Errorf("sub-agent failed: %w", err)
	}
	used := res.ModelName
	if used == "" {
		used = model
	}
	slog.Info("sub-agent completed",
		"model", used, "depth", depth+1,
		"prompt_tokens", res.Usage.PromptTokens,
		"completion_tokens", res.Usage.CompletionTokens)
	return map[string]interface{}{
		"model_used":        used,
		"reply":             res.Reply,
		"prompt_tokens":     res.Usage.PromptTokens,
		"completion_tokens": res.Usage.CompletionTokens,
	}, nil
}

// NewDelegateTool returns just the single `delegate` tool. Retained for
// callers (and tests) that want one tool; NewSubAgentTools returns the
// full set including parallel fan-out.
func NewDelegateTool(deps DelegateDeps) ToolDefinition {
	return NewSubAgentTools(deps)[0]
}

// NewSubAgentTools returns the sub-agent tool set: `delegate` (one task)
// and `delegate_parallel` (several tasks at once). The `delegate` value
// is self-referential — child registries get it registered (capped by
// depth) so a child can delegate once more.
func NewSubAgentTools(deps DelegateDeps) []ToolDefinition {
	maxDepth := deps.MaxDepth
	if maxDepth <= 0 {
		maxDepth = defaultSubAgentMaxDepth
	}

	var delegateDef ToolDefinition
	delegateDef = ToolDefinition{
		Name: "delegate",
		Description: "Delegate a focused sub-task to a child agent running on a chosen model. " +
			"Use for work that benefits from a different model (a heavyweight local or cloud model for hard reasoning, " +
			"a fast local model for bulk/simple work) or an isolated context. The child returns a self-contained result. " +
			"Prefer doing simple things yourself; delegate when the sub-task is substantial or wants a different model.",
		SkillName:  "subagent",
		Parameters: delegateParamsSchema(),
		Execute: func(ctx context.Context, params map[string]interface{}, tctx ToolContext) (interface{}, error) {
			depth := depthFromCtx(ctx)
			if depth >= maxDepth {
				return nil, fmt.Errorf("delegation depth limit (%d) reached — complete this task yourself rather than delegating further", maxDepth)
			}
			spec := parseSpec(params)
			if spec.task == "" {
				return nil, fmt.Errorf("delegate: 'task' is required")
			}
			return runChild(ctx, deps, maxDepth, delegateDef, depth, tctx, spec)
		},
	}

	parallelDef := ToolDefinition{
		Name: "delegate_parallel",
		Description: "Delegate several independent sub-tasks at once, each to its own child agent, run concurrently. " +
			"Use when the tasks don't depend on each other (e.g. research three topics in parallel). Returns one result per task, in order. " +
			"For dependent or single tasks use `delegate` instead.",
		SkillName: "subagent",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"tasks": map[string]interface{}{
					"type":        "array",
					"description": "The independent sub-tasks to run in parallel (max 8). Each is an object like the `delegate` params.",
					"items":       delegateParamsSchema(),
				},
			},
			"required": []string{"tasks"},
		},
		Execute: func(ctx context.Context, params map[string]interface{}, tctx ToolContext) (interface{}, error) {
			depth := depthFromCtx(ctx)
			if depth >= maxDepth {
				return nil, fmt.Errorf("delegation depth limit (%d) reached — complete these tasks yourself", maxDepth)
			}
			rawTasks, ok := params["tasks"].([]interface{})
			if !ok || len(rawTasks) == 0 {
				return nil, fmt.Errorf("delegate_parallel: 'tasks' must be a non-empty array")
			}
			if len(rawTasks) > maxParallelTasks {
				return nil, fmt.Errorf("delegate_parallel: too many tasks (%d); max is %d — split into batches", len(rawTasks), maxParallelTasks)
			}
			specs := make([]childSpec, 0, len(rawTasks))
			for i, rt := range rawTasks {
				m, ok := rt.(map[string]interface{})
				if !ok {
					return nil, fmt.Errorf("delegate_parallel: tasks[%d] is not an object", i)
				}
				s := parseSpec(m)
				if s.task == "" {
					return nil, fmt.Errorf("delegate_parallel: tasks[%d] missing 'task'", i)
				}
				specs = append(specs, s)
			}
			return runParallel(ctx, deps, maxDepth, delegateDef, depth, tctx, specs), nil
		},
	}

	return []ToolDefinition{delegateDef, parallelDef}
}

// runParallel runs specs concurrently with a bounded worker pool and
// returns {"results": [...]} preserving input order. A failed child
// yields an {index, error} entry rather than aborting the whole batch —
// partial results are more useful to the parent than none.
func runParallel(ctx context.Context, deps DelegateDeps, maxDepth int, self ToolDefinition, depth int, tctx ToolContext, specs []childSpec) map[string]interface{} {
	results := make([]map[string]interface{}, len(specs))
	sem := make(chan struct{}, maxParallelWorkers)
	var wg sync.WaitGroup
	for i := range specs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out, err := runChild(ctx, deps, maxDepth, self, depth, tctx, specs[i])
			if err != nil {
				results[i] = map[string]interface{}{"index": i, "error": err.Error(), "task": preview(specs[i].task, 80)}
				return
			}
			out["index"] = i
			out["task"] = preview(specs[i].task, 80)
			results[i] = out
		}(i)
	}
	wg.Wait()
	return map[string]interface{}{"results": results}
}

// delegateParamsSchema is the shared param schema for a single delegated
// task (used by `delegate` directly and as the item shape for
// `delegate_parallel`).
func delegateParamsSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"task": map[string]interface{}{
				"type":        "string",
				"description": "The complete, self-contained instruction for the sub-agent.",
			},
			"model": map[string]interface{}{
				"type":        "string",
				"description": "Model name to run the sub-agent on (see /models). Omit or use \"auto\" to let the router pick based on the task.",
			},
			"difficulty": map[string]interface{}{
				"type":        "string",
				"enum":        []string{"normal", "hard"},
				"description": "\"hard\" prefers a strong model (cloud if a key is configured, else the best local) when the model is auto-picked. Ignored if `model` is set explicitly.",
			},
			"tools": map[string]interface{}{
				"type":        "array",
				"items":       map[string]interface{}{"type": "string"},
				"description": "Builtin skills the sub-agent may use (e.g. web-search, file-editor, shell). Omit for a safe read-only set (web-search, grep, glob, read_file, list_files).",
			},
			"context": map[string]interface{}{
				"type":        "string",
				"description": "Optional background the sub-agent should know but that isn't part of the task instruction itself.",
			},
		},
		"required": []string{"task"},
	}
}

func parseSpec(params map[string]interface{}) childSpec {
	return childSpec{
		task:       strings.TrimSpace(asString(params["task"])),
		model:      asString(params["model"]),
		tools:      asStringSlice(params["tools"]),
		difficulty: asString(params["difficulty"]),
		context:    asString(params["context"]),
	}
}

func preview(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func asString(v interface{}) string {
	s, _ := v.(string)
	return s
}

// asStringSlice coerces a JSON array param (which decodes to []interface{})
// into []string, dropping non-string / empty elements.
func asStringSlice(v interface{}) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
