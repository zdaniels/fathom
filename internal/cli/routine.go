package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/zdaniels/fathom/internal/cli/ui"
)

func init() {
	subcommands = append(subcommands, newRoutineCommand)
}

// newRoutineCommand wires `fathom routine {add,list,show,run,delete}` — named,
// reusable routines you can run on demand or schedule by name.
func newRoutineCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "routine",
		Short: "Save reusable routines you can run or schedule by name",
		Long: `A routine is a named task — a prompt, optionally pinned to one
skill — that you can run on demand or attach to a schedule.

    fathom routine add briefing "summarize my unread email + open PRs"
    fathom routine add inbox --skill gmail "summarize anything unread"
    fathom routine run briefing
    fathom schedule add briefing --every weekday --at 9am`,
	}
	root.AddCommand(routineAdd(), routineList(), routineShow(), routineRun(), routineDelete())
	return root
}

func routineAdd() *cobra.Command {
	var skill string
	cmd := &cobra.Command{
		Use:   `add NAME "PROMPT"`,
		Short: "Create or update a routine",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			prompt := strings.TrimSpace(strings.Join(args[1:], " "))
			store, err := openStore()
			if err != nil {
				return err
			}
			defer store.Close()
			existed, err := store.SaveRoutine(name, prompt, skill, time.Now())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out)
			verb := "created"
			if existed {
				verb = "updated"
			}
			ui.SectionHeader(out, ui.Success("Routine "+verb))
			ui.KV(out, "name", name)
			if skill != "" {
				ui.KV(out, "skill", skill)
			}
			ui.KV(out, "prompt", prompt)
			fmt.Fprintln(out)
			ui.Hint(out,
				"Run it now:     fathom routine run "+name,
				"Put on a clock: fathom schedule add "+name+" --every weekday --at 9am",
			)
			fmt.Fprintln(out)
			return nil
		},
	}
	cmd.Flags().StringVar(&skill, "skill", "", "Pin the routine to one skill (e.g. gmail, github)")
	return cmd
}

func routineList() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List saved routines",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openStore()
			if err != nil {
				return err
			}
			defer store.Close()
			rs, err := store.ListRoutines()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out)
			if len(rs) == 0 {
				ui.SectionHeader(out, "No routines yet")
				ui.Hint(out, `Add one with: fathom routine add <name> "<prompt>"`)
				fmt.Fprintln(out)
				return nil
			}
			ui.SectionHeader(out, fmt.Sprintf("%d routine(s)", len(rs)))
			fmt.Fprintln(out)
			for _, r := range rs {
				tag := ""
				if r.Skill != "" {
					tag = "  " + ui.Mute("["+r.Skill+"]")
				}
				fmt.Fprintf(out, "  %s%s\n             %s\n",
					ui.Brand(r.Name), tag, ui.Body(r.Prompt))
			}
			fmt.Fprintln(out)
			return nil
		},
	}
}

func routineShow() *cobra.Command {
	return &cobra.Command{
		Use:   "show NAME",
		Short: "Show a routine's details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openStore()
			if err != nil {
				return err
			}
			defer store.Close()
			r, err := store.GetRoutine(args[0])
			if err != nil {
				return fmt.Errorf("no routine named %q (see 'fathom routine list')", args[0])
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out)
			ui.SectionHeader(out, r.Name)
			if r.Skill != "" {
				ui.KV(out, "skill", r.Skill)
			}
			ui.KV(out, "prompt", r.Prompt)
			ui.KV(out, "created", r.CreatedAt.Format(time.RFC3339))
			fmt.Fprintln(out)
			return nil
		},
	}
}

func routineDelete() *cobra.Command {
	return &cobra.Command{
		Use:   "delete NAME",
		Short: "Delete a routine",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openStore()
			if err != nil {
				return err
			}
			defer store.Close()
			ok, err := store.DeleteRoutine(args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out)
			if !ok {
				return fmt.Errorf("no routine named %q", args[0])
			}
			fmt.Fprintln(out, "  "+ui.Success("Deleted routine "+args[0]))
			fmt.Fprintln(out)
			return nil
		},
	}
}

func routineRun() *cobra.Command {
	return &cobra.Command{
		Use:   "run NAME",
		Short: "Run a routine once, now",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openStore()
			if err != nil {
				return err
			}
			r, gerr := store.GetRoutine(args[0])
			store.Close()
			if gerr != nil {
				return fmt.Errorf("no routine named %q (see 'fathom routine list')", args[0])
			}
			reply, err := invokeOnce(cmd.Context(), r.Prompt, r.Skill)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out)
			ui.SectionHeader(out, "Routine "+r.Name)
			ui.AgentReply(out, reply)
			fmt.Fprintln(out)
			return nil
		},
	}
}
