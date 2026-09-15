// Fathom — a secure, self-hosted AI agent platform.
//
// Single binary entrypoint. Cobra delegates to subcommands in internal/cli.
package main

import (
	"fmt"
	"net"
	"os"
	"runtime/debug"

	"github.com/zdaniels/fathom/internal/cli"
)

// Force Go's pure-Go DNS resolver. On macOS the libc-based resolver
// caches negative lookups aggressively + system-wide; a brand-new
// domain that's looked up once before it has records can be poisoned
// for minutes even after DNS is correct. The pure resolver queries
// directly without going through dscacheutil or mDNSResponder.
func init() {
	net.DefaultResolver.PreferGo = true
}

// version, commit, buildDate are populated at build time via -ldflags:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always) \
//	                  -X main.commit=$(git rev-parse --short HEAD) \
//	                  -X main.buildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
//
// goreleaser injects these automatically. For un-injected builds (e.g.
// `go install`) we fall back to runtime/debug.ReadBuildInfo so the user
// still sees a meaningful version string.
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

func main() {
	if err := cli.NewRoot(versionString()).Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func versionString() string {
	if version != "dev" {
		return fmt.Sprintf("%s (commit %s, built %s)", version, commit, buildDate)
	}
	// No ldflags injection — try to recover from the embedded VCS info.
	if info, ok := debug.ReadBuildInfo(); ok {
		v := info.Main.Version
		if v == "" || v == "(devel)" {
			v = "dev"
		}
		var sha, ts string
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if len(s.Value) >= 7 {
					sha = s.Value[:7]
				} else {
					sha = s.Value
				}
			case "vcs.time":
				ts = s.Value
			}
		}
		if sha == "" {
			return v
		}
		return fmt.Sprintf("%s (commit %s, built %s)", v, sha, ts)
	}
	return "dev"
}
