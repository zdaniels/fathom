package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/zdaniels/fathom/internal/cli/ui"
	"github.com/zdaniels/fathom/internal/skills"
)

func init() {
	subcommands = append(subcommands, newSetupCommand)
}

// newSetupCommand: `fathom setup` — multi-pick integration wizard.
//
// One command, checklist of every bundled skill, user toggles which ones
// they want with space, hits enter, the wizard runs `fathom install` for
// each in sequence. Faster than running install once per skill — and
// less likely to abandon mid-flow after the third "do you want to
// install gmail?" prompt.
//
// After all installs complete, prompts to restart the service so the
// gateway picks up the new skill code.
func newSetupCommand() *cobra.Command {
	var globalFlag bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Interactive multi-pick wizard for installing integrations",
		Long: "Walk through every bundled skill, check the ones you want, then run\n" +
			"the install flow for each in sequence. Equivalent to running\n" +
			"`fathom install` once per skill — but with one decision point upfront\n" +
			"instead of N.\n\n" +
			"For OAuth skills (gmail, calendar, teams, gchat) you'll still need to\n" +
			"do the provider-side OAuth-client setup once. The wizard guides you\n" +
			"through each.",
		RunE: func(cmd *cobra.Command, args []string) error {
			bundled, err := skills.ListBundled()
			if err != nil {
				return err
			}
			if len(bundled) == 0 {
				return fmt.Errorf("no skills bundled in this binary — rebuild from source")
			}

			m := initialSetupModel(bundled, globalFlag)
			p := tea.NewProgram(m)
			out, err := p.Run()
			if err != nil {
				return err
			}
			done := out.(setupModel)
			if done.cancelled {
				fmt.Fprintln(cmd.OutOrStdout(), "  Cancelled.")
				return nil
			}
			picked := done.picked()
			if len(picked) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "  No skills picked. Nothing to do.")
				return nil
			}

			// Run install for each picked skill, in sequence. We can't
			// reuse the bubbletea TUI for the install loop because the
			// install flow needs to read interactively (paste tokens,
			// click-through OAuth, scan QR) — those don't fit in a TUI.
			// So we exit the TUI cleanly and drop back into normal stdio.
			ctx := cmd.Context()
			out2 := cmd.OutOrStdout()
			for i, name := range picked {
				fmt.Fprintln(out2)
				ui.SectionHeader(out2, fmt.Sprintf("[%d/%d] Installing %s", i+1, len(picked), name))
				if err := installSkill(ctx, out2, name, globalFlag); err != nil {
					fmt.Fprintln(out2, "  "+ui.Error(name+" install failed: "+err.Error()))
					fmt.Fprintln(out2, "  "+ui.Mute("(continuing with the remaining picks)"))
				}
			}

			fmt.Fprintln(out2)
			ui.SectionHeader(out2, ui.Success("Setup complete"))
			ui.Hint(out2, "Restart the gateway so it picks up the new skills:")
			ui.Hint(out2, "  fathom service restart        (if you installed as a launchd service)")
			ui.Hint(out2, "  — or just Ctrl+C and re-run `fathom start`")
			fmt.Fprintln(out2)
			return nil
		},
	}
	cmd.Flags().BoolVar(&globalFlag, "global", false, "Install to ~/.local/share/fantazm/skills/ (works from any directory)")
	return cmd
}

// setupModel is the bubbletea state for the multi-select wizard.
type setupModel struct {
	skills    []skills.BundledSkill
	selected  map[int]bool
	cursor    int
	cancelled bool
	done      bool
	global    bool
}

func initialSetupModel(bundled []skills.BundledSkill, global bool) setupModel {
	// Sort alphabetically; consistency with `fathom install` listing.
	sort.Slice(bundled, func(i, j int) bool { return bundled[i].Name < bundled[j].Name })
	return setupModel{
		skills:   bundled,
		selected: make(map[int]bool),
		global:   global,
	}
}

func (m setupModel) Init() tea.Cmd { return nil }

func (m setupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch key.Type {
	case tea.KeyCtrlC, tea.KeyEsc:
		m.cancelled = true
		return m, tea.Quit
	case tea.KeyEnter:
		m.done = true
		return m, tea.Quit
	case tea.KeyUp:
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil
	case tea.KeyDown:
		if m.cursor < len(m.skills)-1 {
			m.cursor++
		}
		return m, nil
	case tea.KeySpace:
		m.selected[m.cursor] = !m.selected[m.cursor]
		return m, nil
	case tea.KeyRunes:
		switch string(key.Runes) {
		case "j":
			if m.cursor < len(m.skills)-1 {
				m.cursor++
			}
		case "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "a":
			// Toggle all on/off based on majority current state.
			anyOff := false
			for i := range m.skills {
				if !m.selected[i] {
					anyOff = true
					break
				}
			}
			for i := range m.skills {
				m.selected[i] = anyOff
			}
		}
	}
	return m, nil
}

func (m setupModel) View() string {
	if m.done || m.cancelled {
		return ""
	}
	var b strings.Builder
	accent := lipgloss.NewStyle().Foreground(lipgloss.Color("215")).Bold(true)
	body := lipgloss.NewStyle().Foreground(lipgloss.Color("253"))
	mute := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	cobalt := lipgloss.NewStyle().Foreground(lipgloss.Color("67"))

	b.WriteString("\n  " + accent.Render("fathom") + "  " + mute.Render("· setup") + "\n\n")
	b.WriteString("  " + body.Render("Pick the integrations you want to install.") + "\n\n")

	for i, s := range m.skills {
		mark := " "
		if m.selected[i] {
			mark = cobalt.Render("✓")
		}
		row := fmt.Sprintf("  [%s] %-12s %s", mark, s.Name, mute.Render(truncate(s.Manifest.Description, 60)))
		if i == m.cursor {
			row = lipgloss.NewStyle().Background(lipgloss.Color("236")).Render(row)
		}
		b.WriteString(row + "\n")
	}
	b.WriteString("\n")
	b.WriteString("  " + mute.Render("↑/↓ navigate · space toggle · a select all · enter confirm · esc cancel") + "\n")
	if m.global {
		b.WriteString("  " + mute.Render("--global: installs to ~/.local/share/fantazm/skills/") + "\n")
	}
	return b.String()
}

func (m setupModel) picked() []string {
	var out []string
	for i, s := range m.skills {
		if m.selected[i] {
			out = append(out, s.Name)
		}
	}
	return out
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// Wire compile-time check that we use the context import.
var _ = context.Background

// Tiny os.Stderr import-keeper for the file-writer pattern used elsewhere.
var _ = os.Stderr
