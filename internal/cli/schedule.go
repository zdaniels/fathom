package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/zdaniels/fathom/internal/agentfactory"
	"github.com/zdaniels/fathom/internal/cli/ui"
	"github.com/zdaniels/fathom/internal/config"
	"github.com/zdaniels/fathom/internal/scheduler"
	"github.com/zdaniels/fathom/pkg/types"
)

func init() {
	subcommands = append(subcommands, newScheduleCommand)
}

// newScheduleCommand: `fathom schedule {add,list,delete,run}`.
func newScheduleCommand() *cobra.Command {
	root := &cobra.Command{Use: "schedule", Short: "Manage scheduled agent jobs"}
	root.AddCommand(scheduleAdd(), scheduleList(), scheduleDelete(), scheduleRun())
	return root
}

func openStore() (*scheduler.Store, error) {
	return scheduler.OpenStore(scheduler.DefaultDBPath())
}

func scheduleAdd() *cobra.Command {
	var every, at, skill string
	cmd := &cobra.Command{
		Use:   `add [CRON|ROUTINE] "PROMPT"`,
		Short: "Schedule a recurring prompt or routine",
		Long: `Schedule a recurring task. Time it with plain-English --every/--at
or a raw 5-field cron expression; the task is a prompt or a saved routine.

    fathom schedule add --every weekday --at 9am "summarize my open PRs"
    fathom schedule add --every 30m "check for new urgent email"
    fathom schedule add briefing --every weekday --at 9am      # a saved routine
    fathom schedule add --skill gmail --every day --at 8am "summarize unread"
    fathom schedule add "0 9 * * 1-5" "what meetings do I have today?"

--every  day, weekday, weekend, hour, week, a weekday name (monday…),
         or an interval like 30m / 2h / 1d.
--at     time of day for day/week cadences: 9am, 9:30am, 17:30.
--skill  pin the job to one skill (e.g. gmail). Overrides a routine's skill.

The scheduler runs inside 'fathom start' — keep it running for jobs to fire.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openStore()
			if err != nil {
				return err
			}
			defer store.Close()

			cron, taskArgs, err := cronAndRest(every, at, args)
			if err != nil {
				return err
			}

			// The task is either a saved routine (a lone token naming one) or
			// a free-text prompt. A routine snapshots its prompt + skill into
			// the job; an explicit --skill always wins.
			prompt, jobSkill, routineName := "", skill, ""
			if len(taskArgs) == 1 {
				if r, gerr := store.GetRoutine(taskArgs[0]); gerr == nil {
					prompt, routineName = r.Prompt, r.Name
					if skill == "" {
						jobSkill = r.Skill
					}
				}
			}
			if prompt == "" {
				prompt = strings.TrimSpace(strings.Join(taskArgs, " "))
			}
			if prompt == "" {
				return fmt.Errorf("give a prompt or a saved routine name to schedule")
			}

			job, err := store.AddWithSkill(cron, prompt, jobSkill, time.Now())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out)
			ui.SectionHeader(out, ui.Success("Job scheduled"))
			ui.KV(out, "id", job.ID[:8])
			ui.KV(out, "cron", job.Cron)
			ui.KV(out, "next", job.NextRunAt.Format(time.RFC3339))
			if routineName != "" {
				ui.KV(out, "routine", routineName)
			}
			if job.Skill != "" {
				ui.KV(out, "skill", job.Skill)
			}
			ui.KV(out, "prompt", job.Prompt)
			fmt.Fprintln(out)
			ui.Hint(out, "Run 'fathom start' to keep the scheduler active.")
			fmt.Fprintln(out)
			return nil
		},
	}
	cmd.Flags().StringVar(&every, "every", "", "Plain-English cadence: day, weekday, weekend, hour, week, monday…, or 30m/2h/1d")
	cmd.Flags().StringVar(&at, "at", "", "Time of day for day/week cadences: 9am, 9:30am, 17:30")
	cmd.Flags().StringVar(&skill, "skill", "", "Pin the job to one skill (e.g. gmail, github)")
	return cmd
}

// cronAndRest resolves the schedule timing and returns the remaining
// positional args (the task — a prompt or routine name). With --every/--at the
// cron is derived and all args are the task; otherwise the first arg is a raw
// cron expression and the rest is the task.
func cronAndRest(every, at string, args []string) (cron string, rest []string, err error) {
	if every != "" || at != "" {
		cron, err = scheduler.FriendlyToCron(every, at)
		if err != nil {
			return "", nil, err
		}
		return cron, args, nil
	}
	if len(args) < 2 {
		return "", nil, fmt.Errorf("give a cron expression and a task, e.g.\n    fathom schedule add \"0 9 * * *\" \"...\"\nor use plain-English timing:\n    fathom schedule add --every day --at 9am \"...\"")
	}
	return args[0], args[1:], nil
}

// invokeOnce builds a one-shot agent and runs a single prompt (optionally
// pinned to one skill via a soft directive), returning the reply. Used by
// `fathom routine run`.
func invokeOnce(ctx context.Context, prompt, skill string) (string, error) {
	cfg := config.LoadConfig("")
	result, err := agentfactory.CreateDefault(cfg, agentfactory.Options{})
	if err != nil {
		return "", err
	}
	defer func() {
		if result.EgressProxy != nil {
			c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = result.EgressProxy.Stop(c)
		}
	}()
	if !result.Ready {
		return result.Description, nil
	}
	return result.Handler(ctx, types.ChannelMessage{
		ChannelType: "routine-run",
		ChannelID:   "manual",
		SenderID:    "manual-run",
		Text:        scheduler.SkillDirective(skill, prompt),
		Timestamp:   time.Now().UTC(),
	}, makeLocalSession())
}

func scheduleList() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List scheduled jobs",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openStore()
			if err != nil {
				return err
			}
			defer store.Close()
			jobs, err := store.List()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out)
			if len(jobs) == 0 {
				ui.SectionHeader(out, "No scheduled jobs")
				ui.Hint(out, `Add one with: fathom schedule add "<cron>" "<prompt>"`)
				fmt.Fprintln(out)
				return nil
			}
			ui.SectionHeader(out, fmt.Sprintf("%d scheduled job(s)", len(jobs)))
			ui.KV(out, "store", scheduler.DefaultDBPath())
			fmt.Fprintln(out)
			for _, j := range jobs {
				last := ui.Mute("not yet run")
				if j.LastRunAt != nil {
					last = ui.Mute("last " + j.LastRunAt.Format(time.RFC3339))
				}
				fmt.Fprintf(out, "  %s  %s  %s  %s\n             %s\n",
					ui.Brand(j.ID[:8]),
					ui.Body(fmt.Sprintf("%-20s", j.Cron)),
					ui.Mute("next "+j.NextRunAt.Format(time.RFC3339)),
					last,
					ui.Body(j.Prompt),
				)
			}
			fmt.Fprintln(out)
			return nil
		},
	}
}

func scheduleDelete() *cobra.Command {
	return &cobra.Command{
		Use:   "delete IDPREFIX",
		Short: "Remove a scheduled job by ID prefix",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openStore()
			if err != nil {
				return err
			}
			defer store.Close()
			id, err := resolveIDPrefix(store, args[0])
			if err != nil {
				return err
			}
			if _, err := store.Delete(id); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out)
			fmt.Fprintln(out, "  "+ui.Success("Deleted "+id[:8]))
			fmt.Fprintln(out)
			return nil
		},
	}
}

func scheduleRun() *cobra.Command {
	return &cobra.Command{
		Use:   "run IDPREFIX",
		Short: "Force-run a scheduled job once",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openStore()
			if err != nil {
				return err
			}
			defer store.Close()
			id, err := resolveIDPrefix(store, args[0])
			if err != nil {
				return err
			}
			cfg := config.LoadConfig("")
			result, err := agentfactory.CreateDefault(cfg, agentfactory.Options{})
			if err != nil {
				return err
			}
			defer func() {
				if result.EgressProxy != nil {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					_ = result.EgressProxy.Stop(ctx)
				}
			}()
			if !result.Ready {
				fmt.Printf("  %s\n", result.Description)
				return nil
			}
			sched := scheduler.New(scheduler.Options{
				Store: store,
				Invoker: func(ctx context.Context, prompt string) (string, error) {
					return result.Handler(ctx, types.ChannelMessage{
						ChannelType: "scheduler-run",
						ChannelID:   "manual",
						SenderID:    "manual-run",
						Text:        prompt,
						Timestamp:   time.Now().UTC(),
					}, makeLocalSession())
				},
				Deliverer: scheduler.ConsoleDeliverer,
			})
			_, err = sched.RunNow(cmd.Context(), id)
			return err
		},
	}
}

func resolveIDPrefix(store *scheduler.Store, prefix string) (string, error) {
	jobs, err := store.List()
	if err != nil {
		return "", err
	}
	var matches []string
	for _, j := range jobs {
		if strings.HasPrefix(j.ID, prefix) {
			matches = append(matches, j.ID)
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no job matches id prefix %q", prefix)
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("ambiguous: %d jobs match %q, give more digits", len(matches), prefix)
	}
	return matches[0], nil
}

// _ silences unused import errors during partial builds.
var _ = os.Getenv
