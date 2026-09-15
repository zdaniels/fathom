package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/zdaniels/fathom/internal/threads"
)

func init() {
	subcommands = append(subcommands, newExportCommand)
}

// newExportCommand: `fathom export` — dump every thread + message to a
// JSON archive. The output is the user's data in a human-readable,
// portable format that can be re-imported (later) or just kept as a
// backup. Default destination is stdout; --out writes to a file.
//
// We intentionally don't include the vault here. Secrets are
// machine-scoped and including them in the archive would invite the
// user to paste their data export into a Discord channel. If you
// want to back up secrets, copy `~/.fantazm/vault` separately.
func newExportCommand() *cobra.Command {
	var outPath string
	var pretty bool

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export every thread + message to a JSON archive",
		Long: `Writes the gateway's thread/message database to a JSON archive.

By default the archive goes to stdout — pipe it where you want:

    fathom export > fathom-backup.json
    fathom export | jq .

Or write to a file directly:

    fathom export --out fathom-backup.json

The archive contains every non-deleted thread, with every message in
chronological order. It does NOT contain your vault (secrets), device
list, audit log, or scheduled tasks — those have separate purposes and
their own backup paths.

The format is stable + versioned (` + "`schema: \"fathom-export@1\"`" + `) so a
future ` + "`fathom import`" + ` can round-trip it.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := threads.OpenStore(threads.DefaultDBPath())
			if err != nil {
				return fmt.Errorf("open threads db: %w", err)
			}
			defer store.Close()

			archive, err := buildArchive(store)
			if err != nil {
				return err
			}

			var out io.Writer = cmd.OutOrStdout()
			if outPath != "" {
				f, err := os.Create(outPath)
				if err != nil {
					return fmt.Errorf("create %s: %w", outPath, err)
				}
				defer f.Close()
				out = f
			}

			enc := json.NewEncoder(out)
			if pretty || outPath == "" {
				// Pretty by default when going to stdout (humans read
				// it); compact when --out path is given (assume tooling).
				enc.SetIndent("", "  ")
			}
			if err := enc.Encode(archive); err != nil {
				return fmt.Errorf("encode archive: %w", err)
			}
			if outPath != "" {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"exported %d threads (%d messages) → %s\n",
					len(archive.Threads), countMessages(archive.Threads), outPath)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&outPath, "out", "o", "", "Write archive to file instead of stdout")
	cmd.Flags().BoolVar(&pretty, "pretty", false, "Pretty-print JSON even when writing to a file")
	return cmd
}

// exportArchive is the on-disk shape of a `fathom export`. Bump
// Schema when adding/removing top-level fields; keep field-level
// JSON keys stable forever (downstream tools depend on them).
type exportArchive struct {
	Schema     string       `json:"schema"`
	ExportedAt time.Time    `json:"exported_at"`
	Threads    []threadDump `json:"threads"`
}

type threadDump struct {
	ID        string            `json:"id"`
	UserID    string            `json:"user_id"`
	Title     string            `json:"title"`
	Model     string            `json:"model,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	Messages  []threads.Message `json:"messages"`
}

func buildArchive(store *threads.Store) (exportArchive, error) {
	all, err := store.All()
	if err != nil {
		return exportArchive{}, fmt.Errorf("list threads: %w", err)
	}
	dumps := make([]threadDump, 0, len(all))
	for _, t := range all {
		msgs, err := store.AllMessages(t.ID)
		if err != nil {
			return exportArchive{}, fmt.Errorf("messages for %s: %w", t.ID, err)
		}
		dumps = append(dumps, threadDump{
			ID:        t.ID,
			UserID:    t.UserID,
			Title:     t.Title,
			Model:     t.Model,
			CreatedAt: t.CreatedAt,
			UpdatedAt: t.UpdatedAt,
			Messages:  msgs,
		})
	}
	return exportArchive{
		Schema:     "fathom-export@1",
		ExportedAt: time.Now().UTC(),
		Threads:    dumps,
	}, nil
}

func countMessages(dumps []threadDump) int {
	n := 0
	for _, t := range dumps {
		n += len(t.Messages)
	}
	return n
}
