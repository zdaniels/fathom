package scheduler

import (
	"fmt"
	"strings"
)

// SkillDirective wraps a prompt so the agent uses only the named skill. It's a
// soft constraint — a strong instruction, not a hard tool restriction — which
// keeps a scheduled "inbox" routine from wandering into unrelated tools while
// staying compatible with the existing single-prompt Invoker. An empty skill
// returns the prompt unchanged.
func SkillDirective(skill, prompt string) string {
	if strings.TrimSpace(skill) == "" {
		return prompt
	}
	return fmt.Sprintf(
		"Use the %q skill (and only it) to do the following. If that skill "+
			"cannot do it, say so briefly rather than reaching for other tools.\n\n%s",
		skill, prompt,
	)
}
