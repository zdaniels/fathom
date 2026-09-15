package cli

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/zdaniels/fathom/internal/threads"
)

func init() {
	subcommands = append(subcommands, newThreadsCommand)
}

// newThreadsCommand wires `fathom threads`. Three modes:
//
//	fathom threads          → interactive picker → opens chat for the chosen thread
//	fathom threads -r       → resume the most recent thread (no picker)
//	fathom threads list     → non-interactive listing, scripting-friendly
//
// All three read from ~/.fantazm/threads.db — the same store the
// gateway (web + mobile clients) uses, so threads created in one
// surface show up in the others.
func newThreadsCommand() *cobra.Command {
	var resume bool
	cmd := &cobra.Command{
		Use:   "threads",
		Short: "List, pick, or resume chat threads",
		Long: `Pick a chat thread to resume, or jump straight to the most
recent one with -r. Sessions are stored in ~/.fantazm/threads.db
(shared with the web + mobile clients).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ts, err := threads.OpenStore(threads.DefaultDBPath())
			if err != nil {
				return fmt.Errorf("open threads store: %w", err)
			}
			defer ts.Close()

			session := makeLocalSession()
			list, err := ts.List(session.UserID, 50)
			if err != nil {
				return fmt.Errorf("list threads: %w", err)
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(),
					"No threads yet — run `fathom` to start one.")
				return nil
			}

			// -r short-circuits the picker.
			if resume {
				return runChatSession(cmd, "", list[0].ID, false)
			}

			// Interactive picker.
			p := tea.NewProgram(newThreadsPickerModel(list))
			out, err := p.Run()
			if err != nil {
				return err
			}
			picker, _ := out.(threadsPickerModel)
			if picker.cancelled || picker.selected < 0 {
				return nil
			}
			return runChatSession(cmd, "", list[picker.selected].ID, false)
		},
	}
	cmd.Flags().BoolVarP(&resume, "resume", "r", false, "Resume the most recent thread without prompting")

	// Sub: `fathom threads list` — for scripts / quick scans without
	// dropping into a TUI.
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "Print the thread list as plain text",
		RunE: func(cmd *cobra.Command, args []string) error {
			ts, err := threads.OpenStore(threads.DefaultDBPath())
			if err != nil {
				return err
			}
			defer ts.Close()
			session := makeLocalSession()
			list, err := ts.List(session.UserID, 50)
			if err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no threads)")
				return nil
			}
			for _, t := range list {
				title := t.Title
				if title == "" {
					title = "(untitled)"
				}
				when := humanTime(t.UpdatedAt)
				fmt.Fprintf(cmd.OutOrStdout(), "%s  %-12s  %s\n", t.ID, when, title)
			}
			return nil
		},
	})

	return cmd
}

// threadsPickerModel is the bubbletea picker. Up/Down or k/j to move,
// Enter to choose, q or Esc to cancel.
type threadsPickerModel struct {
	threads   []threads.Thread
	cursor    int
	selected  int
	cancelled bool
	width     int
}

func newThreadsPickerModel(list []threads.Thread) threadsPickerModel {
	return threadsPickerModel{threads: list, selected: -1, width: 100}
}

func (m threadsPickerModel) Init() tea.Cmd { return nil }

func (m threadsPickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc", "q":
			m.cancelled = true
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
			return m, nil
		case "down", "j":
			if m.cursor < len(m.threads)-1 {
				m.cursor++
			}
			return m, nil
		case "enter":
			m.selected = m.cursor
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m threadsPickerModel) View() string {
	var b strings.Builder
	headerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("215")).Bold(true)
	rowStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("253"))
	cursorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("117")).Bold(true)
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color("245"))

	b.WriteString("\n")
	b.WriteString(headerStyle.Render("  Pick a thread to resume"))
	b.WriteString("\n")
	b.WriteString(muted.Render("  ↑/↓ move · Enter to open · Esc or q to cancel"))
	b.WriteString("\n\n")

	// Column budgets — pad title to whatever space remains after id + time.
	titleWidth := m.width - 35
	if titleWidth < 20 {
		titleWidth = 20
	}

	for i, t := range m.threads {
		title := t.Title
		if title == "" {
			title = "(untitled)"
		}
		if runeLen(title) > titleWidth {
			title = trimToWidth(title, titleWidth-1) + "…"
		}
		when := humanTime(t.UpdatedAt)
		idShort := t.ID
		if len(idShort) > 8 {
			idShort = idShort[:8]
		}
		row := fmt.Sprintf("  %s  %-12s  %s", idShort, when, title)
		if i == m.cursor {
			b.WriteString(cursorStyle.Render("▶ " + strings.TrimLeft(row, " ")))
		} else {
			b.WriteString(rowStyle.Render(row))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return b.String()
}

// humanTime returns a short relative label like "2m ago" / "3h ago" /
// "yesterday" / "2026-05-20", aimed at scan-readable rows.
func humanTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}

// runeLen counts visible runes (not bytes) for width-checks against
// terminal columns.
func runeLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

// trimToWidth truncates s to at most w visible runes.
func trimToWidth(s string, w int) string {
	if w <= 0 {
		return ""
	}
	n := 0
	for i := range s {
		if n == w {
			return s[:i]
		}
		n++
	}
	return s
}
