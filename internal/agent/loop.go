package agent

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/zdaniels/fathom/internal/agent/llm"
	"github.com/zdaniels/fathom/internal/security"
	"github.com/zdaniels/fathom/pkg/types"
)

const defaultMaxIterations = 10

// Loop orchestrates a single user message → agent response. It calls the LLM
// in a tool-loop: model emits tool calls → registry executes → results go
// back as messages → loop until the model finishes (or we hit MaxIterations).
//
// When a Router is set, ProcessWithModel can dispatch to any named provider;
// the default Process uses the router's default. When only Provider is set
// (legacy single-model setup), all calls go to it.
type Loop struct {
	provider      llm.Provider
	router        *llm.Router
	tools         *ToolRegistry
	mesh          *security.Mesh
	persona       string
	maxIterations int
	tracer        Tracer
}

// Tracer is the optional telemetry hook. nil-safe: Loop calls trace
// methods unconditionally; the no-op tracer below handles the
// "telemetry not configured" case so the loop body stays clean.
//
// The concrete impl lives in internal/integrations/beaconclient. Defined
// as an interface here so the agent package doesn't import it (keeps
// dependency direction one-way: integrations → agent never, agent →
// nothing-special).
type Tracer interface {
	StartSpan(ctx context.Context, name string) Span
	StartChild(parent Span, name string) Span
}

// Span is a single tracked operation. SetAttr/SetError build metadata;
// End closes the span and (eventually) ships it.
type Span interface {
	SetAttr(key string, val any) Span
	SetError(msg string) Span
	End()
	TraceID() string
}

// noopTracer is the default when no tracer is configured. All methods
// return noopSpan so the loop body never has to nil-check.
type noopTracer struct{}
type noopSpan struct{}

func (noopTracer) StartSpan(_ context.Context, _ string) Span { return noopSpan{} }
func (noopTracer) StartChild(_ Span, _ string) Span           { return noopSpan{} }
func (noopSpan) SetAttr(_ string, _ any) Span                 { return noopSpan{} }
func (noopSpan) SetError(_ string) Span                       { return noopSpan{} }
func (noopSpan) End()                                         {}
func (noopSpan) TraceID() string                              { return "" }

// LoopOptions configures a new Loop. Provide either Provider (single-model)
// or Router (multi-model). If both are set, Router takes precedence.
type LoopOptions struct {
	Provider      llm.Provider
	Router        *llm.Router
	Tools         *ToolRegistry
	Mesh          *security.Mesh
	Persona       string
	MaxIterations int

	// Tracer, when set, receives one span per agent iteration with
	// per-tool-call children. Nil falls back to a no-op tracer.
	Tracer Tracer
}

// NewLoop returns a loop with sensible defaults.
func NewLoop(opts LoopOptions) *Loop {
	max := opts.MaxIterations
	if max <= 0 {
		max = defaultMaxIterations
	}
	tracer := opts.Tracer
	if tracer == nil {
		tracer = noopTracer{}
	}
	return &Loop{
		provider:      opts.Provider,
		router:        opts.Router,
		tools:         opts.Tools,
		mesh:          opts.Mesh,
		persona:       opts.Persona,
		maxIterations: max,
		tracer:        tracer,
	}
}

// ProcessResult is the rich return shape from ProcessV2. Usage is the
// cumulative token count across every llm.Chat call this turn made
// (possibly zero if the provider didn't report any — Ollama for
// example doesn't surface usage in the same shape). ModelName is
// the router-resolved name actually used.
type ProcessResult struct {
	Reply     string
	Usage     llm.Usage
	ModelName string
}

// Process runs the agent loop using the router's default model. Equivalent
// to ProcessWithModel(ctx, msg, session, "").
func (l *Loop) Process(ctx context.Context, msg types.ChannelMessage, session types.Session) (string, error) {
	return l.ProcessWithModel(ctx, msg, session, "")
}

// ProcessWithModel runs the agent loop for one inbound message with an
// explicit model selection. modelName "" → router default → fallback to
// the legacy Provider field. Returns the agent's final reply.
func (l *Loop) ProcessWithModel(ctx context.Context, msg types.ChannelMessage, session types.Session, modelName string) (string, error) {
	r, err := l.ProcessV2(ctx, msg, session, modelName)
	return r.Reply, err
}

// ProcessV2 is the rich variant of ProcessWithModel — same behavior
// but returns aggregated token usage + the resolved model name
// alongside the reply. Used by callers that want to persist
// per-message cost info (the threads API).
func (l *Loop) ProcessV2(ctx context.Context, msg types.ChannelMessage, session types.Session, modelName string) (ProcessResult, error) {
	provider, err := l.resolveProvider(modelName)
	if err != nil {
		return ProcessResult{}, err
	}
	reply, usage, err := l.processWithProviderUsage(ctx, msg, session, provider)
	if err != nil {
		return ProcessResult{}, err
	}
	// Resolve the canonical model name when we went through the router.
	resolved := modelName
	if resolved == "" && l.router != nil {
		_, resolved = l.router.Default()
	}
	return ProcessResult{Reply: reply, Usage: usage, ModelName: resolved}, nil
}

// resolveProvider picks the right Provider for this call. Prefers router
// (multi-model setups); falls back to the loop's single Provider field.
func (l *Loop) resolveProvider(name string) (llm.Provider, error) {
	if l.router != nil {
		return l.router.Get(name)
	}
	if l.provider == nil {
		return nil, fmt.Errorf("agent: no LLM provider configured")
	}
	return l.provider, nil
}

// processWithProvider preserves the original signature for callers
// (notably the /review path) that don't need usage data.
func (l *Loop) processWithProvider(ctx context.Context, msg types.ChannelMessage, session types.Session, provider llm.Provider) (string, error) {
	reply, _, err := l.processWithProviderUsage(ctx, msg, session, provider)
	return reply, err
}

// processWithProviderUsage is the real loop body. Aggregates the
// per-iteration Usage from each LLM call into a single total so the
// caller can persist it on the agent message + show cumulative spend.
func (l *Loop) processWithProviderUsage(ctx context.Context, msg types.ChannelMessage, session types.Session, provider llm.Provider) (string, llm.Usage, error) {
	var totalUsage llm.Usage
	reply, err := l.processWithProviderInner(ctx, msg, session, provider, &totalUsage)
	return reply, totalUsage, err
}

// processWithProviderInner is the real loop body, parameterised on which
// provider to call and an out-Usage to accumulate into.
//
// ProcessWithModel and the /review path both go through here.
func (l *Loop) processWithProviderInner(ctx context.Context, msg types.ChannelMessage, session types.Session, provider llm.Provider, outUsage *llm.Usage) (string, error) {
	root := l.tracer.StartSpan(ctx, "agent.process")
	root.SetAttr("session.id", session.ID).
		SetAttr("user.id", session.UserID).
		SetAttr("channel", string(msg.ChannelType)).
		SetAttr("input.length", len(msg.Text))
	defer root.End()

	// Rate-limit check.
	if l.mesh.RateLimiter != nil {
		if ok, wait := l.mesh.RateLimiter.Allow("user:" + session.UserID); !ok {
			root.SetAttr("outcome", "rate_limited")
			return fmt.Sprintf("Rate limited. Try again in %s.", wait), nil
		}
	}

	// Sanitize input. Tag + pass — we don't block.
	sanitized := msg.Text
	if l.mesh.Sanitizer != nil {
		res := l.mesh.Sanitizer.Enforce(msg.Text)
		sanitized = res.Cleaned
		if l.mesh.Audit != nil {
			l.mesh.Audit.Log(session.ID, session.UserID, types.AuditLLMRequest,
				map[string]interface{}{
					"channel":    msg.ChannelType,
					"textLength": len(sanitized),
					"detections": res.Detections,
					"risk":       res.Risk,
				}, types.PolicyAllow)
		}
	}

	toolDefs := l.tools.LLMDefs()
	cb := NewContextBuilder(l.persona).
		SetToolCatalog(toolDefs).
		AddUserMessage(sanitized)
	messages := cb.Build(session)

	var lastContent string
	var lastToolName string
	var lastToolResult string
	for iter := 0; iter < l.maxIterations; iter++ {
		slog.Debug("agent loop iteration", "iter", iter+1, "messageCount", len(messages))

		iterSpan := l.tracer.StartChild(root, "agent.iteration")
		iterSpan.SetAttr("iter", iter+1).
			SetAttr("message.count", len(messages))

		llmSpan := l.tracer.StartChild(iterSpan, "llm.chat")
		llmSpan.SetAttr("tool.count", len(toolDefs))

		var resp llm.Response
		var err error
		if len(toolDefs) > 0 {
			resp, err = provider.Chat(ctx, messages, toolDefs)
		} else {
			resp, err = provider.Chat(ctx, messages, nil)
		}
		if err != nil {
			llmSpan.SetError(err.Error()).End()
			iterSpan.SetError(err.Error()).End()
			root.SetError(err.Error())
			return "", err
		}
		llmSpan.SetAttr("finish.reason", string(resp.FinishReason)).
			SetAttr("tool.calls", len(resp.ToolCalls))
		if resp.Usage != nil {
			llmSpan.SetAttr("usage.prompt", resp.Usage.PromptTokens).
				SetAttr("usage.completion", resp.Usage.CompletionTokens)
			outUsage.PromptTokens += resp.Usage.PromptTokens
			outUsage.CompletionTokens += resp.Usage.CompletionTokens
		}
		llmSpan.End()
		if l.mesh.Audit != nil {
			detail := map[string]interface{}{
				"finishReason": resp.FinishReason,
				"toolCalls":    len(resp.ToolCalls),
			}
			if resp.Usage != nil {
				detail["usage"] = resp.Usage
			}
			l.mesh.Audit.Log(session.ID, session.UserID, types.AuditLLMResponse, detail, types.PolicyAllow)
		}
		lastContent = resp.Content

		// Final turn: no tool calls → check canary, return.
		if resp.FinishReason != llm.FinishToolCalls || len(resp.ToolCalls) == 0 {
			if l.mesh.Canary != nil {
				if hit := l.mesh.Canary.Check(resp.Content); hit != nil {
					if l.mesh.Audit != nil {
						l.mesh.Audit.Log(session.ID, session.UserID, types.AuditCanaryTriggered,
							map[string]interface{}{"location": "llm_output", "canary": hit.Label},
							types.PolicyDeny)
					}
					iterSpan.SetError("canary_triggered").End()
					root.SetError("canary_triggered").SetAttr("outcome", "canary")
					return "I encountered a security issue processing your request. The session has been flagged for review.", nil
				}
			}
			if resp.Content == "" {
				iterSpan.SetAttr("outcome", "empty").End()
				// Model emitted nothing — but if the last tool ran
				// successfully, surface that instead of a dead-end message.
				if lastToolResult != "" {
					root.SetAttr("outcome", "tool_fallback")
					return formatToolFallback(lastToolName, lastToolResult), nil
				}
				root.SetAttr("outcome", "empty")
				return "I couldn't generate a response.", nil
			}
			iterSpan.SetAttr("outcome", "final").End()
			root.SetAttr("outcome", "ok").SetAttr("iterations", iter+1)
			return resp.Content, nil
		}

		// Mid-loop: append the assistant turn (WITH its tool_calls — the
		// model needs to see what it asked for so it can correlate the
		// tool_result that follows) plus the tool results, then iterate.
		messages = append(messages, llm.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
			Trusted:   true,
		})
		for _, tc := range resp.ToolCalls {
			toolSpan := l.tracer.StartChild(iterSpan, "tool."+tc.Name)
			toolSpan.SetAttr("tool.name", tc.Name).
				SetAttr("tool.call_id", tc.ID)
			result := l.tools.Execute(ctx, tc, session)
			content := ""
			if result.Success {
				content = stringify(result.Output)
				lastToolName = tc.Name
				lastToolResult = content
				toolSpan.SetAttr("tool.output.length", len(content))
			} else {
				content = "Error: " + result.Error
				toolSpan.SetError(result.Error)
			}
			toolSpan.End()
			messages = append(messages, llm.Message{
				Role:       "tool",
				Content:    content,
				ToolCallID: tc.ID,
				Name:       tc.Name,
				Trusted:    true,
			})
		}
		iterSpan.End()
	}

	slog.Warn("agent loop hit max iterations", "max", l.maxIterations, "session", session.ID)
	root.SetAttr("outcome", "max_iterations").SetAttr("iterations", l.maxIterations)
	return "I've reached the maximum number of steps for this request. Here's what I have so far:\n\n" + lastContent, nil
}

func stringify(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	// JSON-marshal anything else.
	b, _ := jsonMarshal(v)
	return string(b)
}

// formatToolFallback renders a tool result for the user when the model
// returned empty content. Trims long JSON outputs so it stays readable.
func formatToolFallback(name, raw string) string {
	const cap = 1500
	body := raw
	if len(body) > cap {
		body = body[:cap] + "\n…(truncated)"
	}
	return fmt.Sprintf("`%s` returned:\n\n%s", name, body)
}
