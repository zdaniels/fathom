package cli

import (
	"fmt"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"
	"github.com/zdaniels/fathom/internal/cli/ui"
	"github.com/zdaniels/fathom/internal/config"
)

func init() {
	subcommands = append(subcommands, newOpenCommand)
}

// newOpenCommand: `fathom open` — point the user's browser at the running
// gateway's chat UI. Doesn't start the gateway itself (that's `fathom
// start`); just opens http://host:port in the OS-default browser.
func newOpenCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "open",
		Short: "Open the web chat UI in your browser",
		Long: `Opens http://host:port/ from your fathom.config.yaml in the OS-default
browser. The gateway must already be running (start it with 'fathom start').
If you haven't authorized the browser yet, you'll be prompted to paste the
API token printed on first gateway boot.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.LoadConfig("")
			host := cfg.Host
			if host == "" || host == "0.0.0.0" {
				host = "127.0.0.1"
			}
			port := cfg.Port
			if port == 0 {
				port = 8790
			}
			url := fmt.Sprintf("http://%s:%d/", host, port)

			out := cmd.OutOrStdout()
			fmt.Fprintln(out)
			ui.Hint(out, "Opening "+url+" in your browser…")
			ui.Hint(out, "If the gateway isn't running yet, start it with: fathom start")
			fmt.Fprintln(out)

			if err := openInBrowser(url); err != nil {
				return fmt.Errorf("couldn't open browser automatically — visit %s manually: %w", url, err)
			}
			return nil
		},
	}
}

// openInBrowser launches the OS-default browser for url. Cross-platform
// shells out to the per-OS canonical opener — darwin: open, linux:
// xdg-open, windows: rundll32.exe url.dll.
func openInBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return fmt.Errorf("don't know how to open a URL on %s", runtime.GOOS)
	}
	return cmd.Start()
}
