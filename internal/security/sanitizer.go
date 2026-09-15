package security

import (
	"regexp"
	"strings"
)

// Sanitizer scans user input for known prompt-injection / jailbreak shapes
// and tags the result so the context builder can wrap suspicious content
// in <untrusted_data> for the model. Tag-and-pass beats hard-block — a
// false positive shouldn't drop the user's question.
type Sanitizer struct {
	patterns []sanitizerPattern
}

type sanitizerPattern struct {
	name    string
	pattern *regexp.Regexp
	weight  int // severity 1-3; sum becomes the risk score
}

// SanitizationResult is what Enforce returns.
type SanitizationResult struct {
	Original   string
	Cleaned    string
	Risk       int
	Detections []string
}

// invisibleRangePattern uses Go regexp's \x{XXXX} escapes so the source file
// stays ASCII (literal BOMs / zero-width chars confuse the tooling).
//
// Ranges covered:
//
//	U+00AD                soft hyphen
//	U+FEFF                BOM
//	U+200B - U+200F       zero-width + LTR/RTL marks
//	U+202A - U+202E       directional formatting
//	U+2060 - U+2069       invisible operators / FSI / PDI
const invisibleRangePattern = `[\x{00AD}\x{FEFF}\x{200B}-\x{200F}\x{202A}-\x{202E}\x{2060}-\x{2069}]`

// NewSanitizer constructs the default pattern set. Mirrors the TS impl one
// pattern at a time so the risk-score totals stay comparable across the
// rewrite.
func NewSanitizer() *Sanitizer {
	raw := []struct {
		name    string
		pattern string
		weight  int
	}{
		{"ignore_previous", `(?i)ignore\s+(all\s+)?previous\s+(instructions|prompts|directives)`, 3},
		{"role_hijack", `(?i)you\s+are\s+(now|actually)\s+(a|an)\s+`, 2},
		{"system_prompt_extract", `(?i)(print|reveal|show|output)\s+(your\s+)?(system\s+)?(prompt|instructions)`, 3},
		{"jailbreak_dan", `(?i)\b(DAN|jailbreak|developer\s+mode|sudo\s+mode)\b`, 3},
		{"prompt_injection_marker", `(?i)\[\[(INST|SYSTEM|/INST)\]\]`, 2},
		{"chat_template_break", `(?i)<\|im_(start|end)\|>|<\|endoftext\|>|</?s>`, 2},
		{"role_marker", `(?i)^\s*(system|assistant|user)\s*:`, 1},
		{"unicode_tag", invisibleRangePattern, 2},
		{"base64_blob", `(?:[A-Za-z0-9+/]{40,}={0,2})`, 1},
		{"data_uri", `(?i)data:[\w/]+;base64,`, 2},
		{"exec_keywords", `(?i)\b(exec|execute|run|eval)\s+(this|the\s+following)\b`, 2},
		{"http_command", `(?i)\b(curl|wget|fetch)\s+http`, 2},
		{"command_substitution", "\\$\\([^)]+\\)|`[^`]+`", 2},
		{"shell_keywords", `(?i)\b(rm\s+-rf|sudo\s+|chmod\s+|chown\s+)\b`, 3},
		{"sql_injection", `(?i)(\bunion\s+select\b|\bdrop\s+table\b|--\s|/\*.*\*/)`, 2},
		{"private_key_marker", `-----BEGIN\s+(RSA|EC|OPENSSH|PGP)\s+PRIVATE\s+KEY-----`, 3},
		{"path_traversal", `(\.\./){3,}`, 2},
		{"role_play_disguise", `(?i)pretend\s+(you\s+are|to\s+be)\s+`, 1},
		{"developer_persona", `(?i)act\s+as\s+(a|an)\s+(developer|admin|root|system)`, 2},
		{"do_anything", `(?i)\bdo\s+anything\s+now\b`, 3},
		{"new_instructions", `(?i)new\s+instructions\s*[:=]`, 2},
		{"override_instructions", `(?i)(override|forget|disregard)\s+(your|all|prior)\s+(instructions|rules|guidelines)`, 3},
		{"hypothetical", `(?i)hypothetically,?\s+(if\s+)?you\s+(could|were)`, 1},
	}

	s := &Sanitizer{patterns: make([]sanitizerPattern, 0, len(raw))}
	for _, p := range raw {
		re, err := regexp.Compile(p.pattern)
		if err != nil {
			logger.Warn("sanitizer pattern failed to compile, skipping", "name", p.name, "err", err)
			continue
		}
		s.patterns = append(s.patterns, sanitizerPattern{name: p.name, pattern: re, weight: p.weight})
	}
	return s
}

// Enforce scans input, returns the result with detections. The Cleaned text
// has zero-width unicode stripped; everything else is preserved (the context
// builder is what tags untrusted content, not the sanitizer).
func (s *Sanitizer) Enforce(input string) SanitizationResult {
	res := SanitizationResult{Original: input}
	for _, p := range s.patterns {
		if p.pattern.MatchString(input) {
			res.Detections = append(res.Detections, p.name)
			res.Risk += p.weight
		}
	}
	res.Cleaned = stripInvisibleUnicode(input)
	return res
}

var invisibleRange = regexp.MustCompile(invisibleRangePattern)

// invisibleRunes is the set we look for in the fast pre-check before running
// the regex. Built from rune literals so the source file stays ASCII.
var invisibleRunes = string([]rune{
	0x00AD,
	0xFEFF,
	0x200B, 0x200C, 0x200D, 0x200E, 0x200F,
	0x202A, 0x202B, 0x202C, 0x202D, 0x202E,
	0x2060, 0x2061, 0x2062, 0x2063, 0x2064, 0x2065, 0x2066, 0x2067, 0x2068, 0x2069,
})

func stripInvisibleUnicode(s string) string {
	if !strings.ContainsAny(s, invisibleRunes) {
		return s
	}
	return invisibleRange.ReplaceAllString(s, "")
}
