// Package ui renders Fathom's terminal output: minimal agent-CLI chrome.
// Plain prose for the agent's reply (no boxes), a one-line wordmark, soft
// amber accent on "Fathom", muted body text. Brand discipline: never use
// more than one accent in a single screen.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// ANSI palette. Two principles:
//  1. Stay legible on both light and dark terminals.
//  2. Use ONE warm accent (amber) for the brand mark, ONE cool accent
//     (cobalt) for structural elements, and everything else is mute.
const (
	reset = "\x1b[0m"
	bold  = "\x1b[1m"

	// Amber — only on the "Fathom" wordmark and on success ticks.
	amberFG = "\x1b[38;5;215m"

	// Cobalt — quiet structure: prompts, rules, key labels.
	cobaltFG = "\x1b[38;5;67m"

	// Body + secondary text.
	textFG = "\x1b[38;5;253m"
	muteFG = "\x1b[38;5;244m"

	// Status colors.
	warnFG  = "\x1b[38;5;215m"
	errorFG = "\x1b[38;5;203m"
)

var enabled = func() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}()

// Width returns the actual terminal width. Falls back to 80 if stdout
// isn't a TTY or the ioctl fails. We deliberately do NOT cap — the user
// expects borders to extend to the edge of their terminal.
func Width() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}

func wrap(s, color string) string {
	if !enabled {
		return s
	}
	return color + s + reset
}

// Brand wraps text in the amber accent. Reserved for "Fathom" + success ticks.
func Brand(s string) string { return wrap(s, amberFG) }

// Accent wraps text in the cool cobalt for structural elements.
func Accent(s string) string { return wrap(s, cobaltFG) }

// Mute renders secondary text in grey.
func Mute(s string) string { return wrap(s, muteFG) }

// Bold renders in bright body color + bold.
func Bold(s string) string { return wrap(s, bold+textFG) }

// Body renders in body color.
func Body(s string) string { return wrap(s, textFG) }

// Success: amber check + text.
func Success(s string) string { return wrap("✓ "+s, amberFG) }

// Warn: amber arrow + text.
func Warn(s string) string { return wrap("! "+s, warnFG) }

// Error: red cross + text.
func Error(s string) string { return wrap("✗ "+s, errorFG) }

// Wordmark prints the Fathom + github.com/zdaniels/fathom line. One-line, lowercase
// tagline in muted grey. No glyph clusters — keep it quiet.
func Wordmark(out io.Writer) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  "+Brand(Bold("fathom"))+"  "+Mute("·")+"  "+Mute("github.com/zdaniels/fathom"))
	fmt.Fprintln(out)
}

// Rule prints a thin horizontal divider, full-width minus indent.
func Rule(out io.Writer) {
	w := Width() - 4
	if w < 20 {
		w = 20
	}
	fmt.Fprintln(out, "  "+Mute(strings.Repeat("─", w)))
}

// StatusLine renders a single subtle metadata line under the wordmark. Pairs
// are rendered as "label value" separated by middle dots.
func StatusLine(out io.Writer, pairs ...[2]string) {
	parts := make([]string, 0, len(pairs)*2)
	for i, p := range pairs {
		if i > 0 {
			parts = append(parts, Mute("·"))
		}
		parts = append(parts, Mute(p[0])+" "+Body(p[1]))
	}
	fmt.Fprintln(out, "  "+strings.Join(parts, "  "))
}

// SectionHeader prints a single-line header with subtle leading accent.
// Used in non-chat commands (init, vault list, schedule list).
func SectionHeader(out io.Writer, title string) {
	fmt.Fprintln(out, "  "+Accent("▎")+" "+Bold(title))
}

// KV renders "key value" with consistent alignment. Used in init summary,
// status output, vault list rows. Keys longer than 12 chars get no
// padding (they push the value over) instead of crashing — used to
// strings.Repeat with a negative count which panicked.
func KV(out io.Writer, key, value string) {
	const maxKey = 12
	pad := ""
	if len(key) < maxKey {
		pad = strings.Repeat(" ", maxKey-len(key))
	}
	fmt.Fprintf(out, "  %s%s  %s\n", Mute(key), pad, Body(value))
}

// PromptBoxTop opens the input box: a rounded top rule that spans the
// terminal width with the standard 2-col indent.
func PromptBoxTop(out io.Writer) {
	fmt.Fprintln(out, "  "+Accent("╭"+strings.Repeat("─", boxInnerWidth())+"╮"))
}

// PromptBoxBottom closes the input box after Enter.
func PromptBoxBottom(out io.Writer) {
	fmt.Fprintln(out, "  "+Accent("╰"+strings.Repeat("─", boxInnerWidth())+"╯"))
}

// UserPrompt prints the inline marker "│ > " between the top and bottom
// borders. The cursor lives here until Enter is pressed; the right edge
// of the input line stays open (no TUI to redraw on each keystroke).
func UserPrompt(out io.Writer) {
	fmt.Fprint(out, "  "+Accent("│ ")+Accent(">")+" ")
}

// boxInnerWidth is the number of "─" cells between the corner glyphs.
// Layout: "  " (indent=2) + "╭" + N×"─" + "╮" = 2 + 1 + N + 1 = N + 4
// chars total. We use Width() - 5 (one extra safety col) because many
// terminals wrap or clip a glyph landing exactly in the last column.
func boxInnerWidth() int {
	n := Width() - 5
	if n < 16 {
		n = 16
	}
	return n
}

// AgentReply prints the agent's reply: indented prose, no box, no label.
// Lines are wrapped at the comfortable width with the standard indent.
func AgentReply(out io.Writer, text string) {
	w := Width() - 4
	if w < 40 {
		w = 40
	}
	for _, line := range wrapLines(text, w) {
		fmt.Fprintln(out, "  "+Body(line))
	}
}

// Hint prints dim "next-step" lines, indented.
func Hint(out io.Writer, lines ...string) {
	for _, l := range lines {
		fmt.Fprintln(out, "  "+Mute(l))
	}
}

// Field prompts inside the init wizard. Single arrow, no label fanfare.
func Field(out io.Writer, prompt string) {
	fmt.Fprint(out, "  "+Mute("›")+" "+Body(prompt))
}

func wrapLines(s string, width int) []string {
	var out []string
	for _, paragraph := range strings.Split(s, "\n") {
		if paragraph == "" {
			out = append(out, "")
			continue
		}
		// Preserve leading whitespace of indented lines (list markers, code).
		leading := ""
		i := 0
		for i < len(paragraph) && (paragraph[i] == ' ' || paragraph[i] == '\t') {
			leading += string(paragraph[i])
			i++
		}
		words := strings.Fields(paragraph[i:])
		var line string
		for _, w := range words {
			candidate := w
			if line != "" {
				candidate = line + " " + w
			} else {
				candidate = leading + w
			}
			if visibleLen(candidate) > width && line != "" {
				out = append(out, line)
				line = leading + w
				continue
			}
			line = candidate
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func visibleLen(s string) int { return len(stripANSI(s)) }

// runeLen counts visible runes — required when working with multi-byte
// glyphs like box-drawing or Figlet characters where len() overcounts.
func runeLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

// stripANSI removes ANSI escape sequences for accurate width counting.
func stripANSI(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			i = j
			continue
		}
		out.WriteByte(s[i])
	}
	return out.String()
}
