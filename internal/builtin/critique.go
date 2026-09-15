package builtin

import (
	"context"
	"fmt"
	"time"

	"github.com/zdaniels/fathom/internal/agent"
	"github.com/zdaniels/fathom/internal/agent/llm"
	"github.com/zdaniels/fathom/pkg/types"
)

// CritiqueTool exposes a "get a second opinion from another model" capability
// to the agent itself. Used mid-turn: the primary agent can call this when
// it's uncertain, has competing options, or wants verification before
// committing to a recommendation.
//
// The critic runs the SAME agent loop with the SAME tool registry — full
// tool access (grep, read_file, bash, etc.) — so it can actually verify the
// claim under review rather than just rephrasing.
//
// Construction is via a factory func because we need the loop + router from
// the agent factory. Returns nil when no router is configured (single-model
// setups have no second model to critique with).
func CritiqueTool(loop critiqueLoop, router *llm.Router) agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "critique",
		Description: `Get a rigorous critique of a claim or proposed answer from a different LLM. Use when you're uncertain, when stakes are high (production code, security claims), or to stress-test a recommendation before committing.

The critic has full tool access and will verify, not just paraphrase. Returns: {critique, criticModel}.

Cost: this makes a separate LLM call. Use sparingly.`,
		SkillName: "critique",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"claim": map[string]interface{}{
					"type":        "string",
					"description": "The claim, answer, or recommendation to critique. Include enough context that the critic can evaluate it without seeing the full conversation.",
				},
				"context": map[string]interface{}{
					"type":        "string",
					"description": "Optional: the user's original question or surrounding context.",
				},
				"critic": map[string]interface{}{
					"type":        "string",
					"description": "Optional: name of the critic model. Defaults to the 'critic' entry, falling back to any model differing from the current one.",
				},
			},
			"required": []string{"claim"},
		},
		Execute: func(ctx context.Context, p map[string]interface{}, tctx agent.ToolContext) (interface{}, error) {
			if router == nil {
				return nil, fmt.Errorf("critique unavailable: configure llm.models with at least two entries (one named 'critic')")
			}
			claim, _ := p["claim"].(string)
			userCtx, _ := p["context"].(string)
			criticOverride, _ := p["critic"].(string)
			if claim == "" {
				return nil, fmt.Errorf("critique: claim is required")
			}

			var criticName string
			if criticOverride != "" {
				if _, err := router.Get(criticOverride); err != nil {
					return nil, fmt.Errorf("critique: %w", err)
				}
				criticName = criticOverride
			} else {
				_, criticName = router.PickCritic("")
			}

			prompt := "You are reviewing a claim for accuracy and quality. Be rigorous — your job is to find what's wrong, missing, or worth challenging. You have full tool access (grep, read_file, bash, etc.) to verify; don't just paraphrase.\n"
			if userCtx != "" {
				prompt += "\nContext / original question:\n" + userCtx + "\n"
			}
			prompt += "\nClaim to critique:\n" + claim + "\n\nProvide a tight critique: (1) what's correct briefly, (2) what's wrong or missing with evidence, (3) concrete improvements. If the claim is solid, say so plainly."

			cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()
			msg := types.ChannelMessage{
				ChannelType: "critique",
				ChannelID:   "internal",
				SenderID:    tctx.UserID,
				Text:        prompt,
				Timestamp:   time.Now().UTC(),
			}
			sess := types.Session{
				ID: tctx.SessionID + "-critic", UserID: tctx.UserID,
				CreatedAt:   time.Now().UTC(),
				ExpiresAt:   time.Now().UTC().Add(time.Hour),
				Permissions: tctx.Permissions,
			}
			reply, err := loop.ProcessWithModel(cctx, msg, sess, criticName)
			if err != nil {
				return nil, fmt.Errorf("critique: %w", err)
			}
			return map[string]interface{}{
				"critique":    reply,
				"criticModel": criticName,
			}, nil
		},
	}
}

// critiqueLoop is the subset of *agent.Loop the critique tool needs. Defined
// as an interface so this file doesn't import the concrete *agent.Loop and
// trip the package-cycle check (agent imports builtin? no — but the factory
// passes the loop in, so we accept any matching interface).
type critiqueLoop interface {
	ProcessWithModel(ctx context.Context, msg types.ChannelMessage, sess types.Session, model string) (string, error)
}
