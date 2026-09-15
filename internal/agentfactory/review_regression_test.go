package agentfactory

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zdaniels/fathom/internal/security"
	"github.com/zdaniels/fathom/pkg/types"
)

func fakeTakeover(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("FATHOM_WORKSPACE_ROOT", dir)
	script := `#!/bin/sh
result=new
for arg in "$@"; do
if [ "$arg" = "--resume" ]; then result=resumed; fi
done
printf '{"subtype":"success","result":"%s","session_id":"provider-session"}' "$result"
`
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
}
func TestTakeoverConversationIsolation(t *testing.T) {
	fakeTakeover(t)
	h, err := BuildTakeoverHandler(types.Config{})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ user, channel, thread, want string }{
		{"alice", "rest", "api", "new"}, {"alice", "rest", "api", "new"}, {"bob", "rest", "api", "new"},
		{"alice", "webchat", "thread", "new"}, {"bob", "webchat", "thread", "new"}, {"alice", "webchat", "thread", "resumed"},
		{"alice", "cli", "thread", "new"}, {"alice", "scheduler", "scheduler", "new"}, {"alice", "scheduler", "scheduler", "new"},
	}
	for _, c := range cases {
		got, err := h(context.Background(), types.ChannelMessage{ChannelType: c.channel, ChannelID: c.thread, Text: "hello"}, types.Session{UserID: c.user})
		if err != nil || got != c.want {
			t.Fatalf("%+v: got %q, %v", c, got, err)
		}
	}
}
func TestTakeoverSerializesConcurrentTurns(t *testing.T) {
	fakeTakeover(t)
	h, err := BuildTakeoverHandler(types.Config{})
	if err != nil {
		t.Fatal(err)
	}
	const n = 16
	results := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := h(context.Background(), types.ChannelMessage{ChannelType: "webchat", ChannelID: "thread", Text: "hi"}, types.Session{UserID: "alice"})
			if err != nil {
				t.Error(err)
			}
			results <- got
		}()
	}
	wg.Wait()
	close(results)
	fresh := 0
	for got := range results {
		if got == "new" {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("%d fresh sessions for one concurrent conversation", fresh)
	}
}
func TestNamedLocalModelDoesNotRequireLegacyKey(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	vault, err := security.OpenVault(security.VaultOpenOptions{Key: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	cfg := types.DefaultConfig()
	cfg.Profile = types.ProfileMinimal
	cfg.LLM.Default = "local"
	cfg.LLM.Models = map[string]types.LLMModelConfig{"local": {Provider: "ollama", Model: "local"}}
	result, err := CreateDefault(cfg, Options{VaultOverride: vault})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ready {
		t.Fatal(result.Description)
	}
}
func TestTakeoverFactoryIgnoresUnusedModelConfig(t *testing.T) {
	fakeTakeover(t)
	vault, err := security.OpenVault(security.VaultOpenOptions{Key: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	cfg := types.DefaultConfig()
	cfg.Takeover = &types.TakeoverConfig{Enabled: true}
	cfg.LLM.Provider = "unused-invalid"
	result, err := CreateDefault(cfg, Options{VaultOverride: vault})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ready || result.HandlerN != nil || result.HandlerNU != nil {
		t.Fatal("incorrect active backend")
	}
	for i := 0; i < 2; i++ {
		reply, err := result.Handler(context.Background(), types.ChannelMessage{ChannelType: "scheduler", ChannelID: "scheduler", Text: "run"}, types.Session{UserID: "scheduler"})
		if err != nil || reply != "new" {
			t.Fatalf("scheduler got %q %v", reply, err)
		}
	}
}
