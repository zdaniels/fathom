package cli

import (
	"github.com/spf13/cobra"
)

// NewRoot wires the top-level fathom command tree. Each subcommand lives in
// its own file in this package and registers itself via init() — keeping the
// root small and the command set self-organising.
//
// Bare `fathom` (no args) launches `fathom chat` — chat is the headline
// flow, so we save the user a word. Subcommands stay explicit:
// `fathom init`, `fathom doctor`, etc.
func NewRoot(version string) *cobra.Command {
	root := &cobra.Command{
		Use:   "fathom",
		Short: "Fathom — your secure local AI agent",
		Long: `Fathom is your local AI agent. Single binary, multi-LLM
(OpenAI / Anthropic / Ollama), encrypted vault, sandboxed skills,
hash-chained audit log, cross-session memory.

  fathom              Launch the chat REPL (same as 'fathom chat')
  fathom init         Setup wizard — per-project, or --global for one-anywhere
  fathom chat         Local TUI chat with your agent
  fathom doctor       Diagnose setup (config, LLM, vault, sidecars)
  fathom start        Run the HTTP gateway + scheduler
  fathom vault        Manage the encrypted secrets vault
  fathom routine      Save reusable routines (run or schedule by name)
  fathom schedule     Manage recurring agent jobs (--every/--at or cron)
  fathom audit        View the hash-chained audit log
  fathom hub          Search the Fathom Secure Hub

  https://github.com/zdaniels/fathom`,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		// When called with no subcommand and no args, defer to chat. Cobra
		// only invokes RunE if Args check passes AND no subcommand matched.
		RunE: func(cmd *cobra.Command, args []string) error {
			// Find the chat subcommand we already registered and run it.
			for _, sub := range cmd.Commands() {
				if sub.Use == "chat" {
					sub.SetContext(cmd.Context())
					return sub.RunE(sub, args)
				}
			}
			return cmd.Help()
		},
	}

	for _, sub := range subcommands {
		root.AddCommand(sub())
	}
	return root
}

// subcommands is the registration point — each subcommand file appends to it
// via an init() block. Lets us keep the root command stable as new commands
// land without touching this file.
var subcommands []func() *cobra.Command
