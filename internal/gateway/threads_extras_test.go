package gateway

import "testing"

func TestAutoTitleFromFirstSentence(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "summarize my unread emails", "summarize my unread emails"},
		{"first sentence", "Hello. There's more. And more.", "Hello"},
		{"question", "What's the capital of France? It's Paris.", "What's the capital of France?"},
		{"long", "this is a really long single sentence with no punctuation that goes on and on and on", "this is a really long single sentence with no punctuation t…"},
		{"empty", "", "New chat"},
		{"whitespace", "   \n\t  ", "New chat"},
		{"with code fence", "fix this:\n```go\nfunc main(){}\n```\nthanks", "fix this: thanks"},
		{"unterminated fence", "ok\n```\nrest dropped", "ok"},
		{"newlines collapsed", "line one\nline two", "line one line two"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := autoTitle(c.in)
			if got != c.want {
				t.Errorf("autoTitle(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
