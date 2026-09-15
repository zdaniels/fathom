package agentfactory

import (
	"testing"

	"github.com/zdaniels/fathom/pkg/types"
)

func TestTakeoverDesc(t *testing.T) {
	cases := []struct {
		tc   *types.TakeoverConfig
		want string
	}{
		{nil, "claude (provider default model)"},
		{&types.TakeoverConfig{Enabled: true}, "claude (provider default model)"},
		{&types.TakeoverConfig{Enabled: true, Provider: "codex"}, "codex (provider default model)"},
		{&types.TakeoverConfig{Enabled: true, Model: "sonnet"}, "claude:sonnet"},
		{&types.TakeoverConfig{Enabled: true, Provider: "claude", Model: "opus"}, "claude:opus"},
	}
	for _, c := range cases {
		if got := TakeoverDesc(c.tc); got != c.want {
			t.Errorf("TakeoverDesc(%+v) = %q, want %q", c.tc, got, c.want)
		}
	}
}
