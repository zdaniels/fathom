package skills

import (
	"context"
	"github.com/zdaniels/fathom/internal/brandenv"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Optional hardening for trusted skill subprocesses. For untrusted code use
// FATHOM_SKILL_SANDBOX=required (sandbox_required.go), which fails closed.
// OS-level isolation for skill subprocesses. The egress proxy already keeps
// skills from seeing raw secrets, but a skill is still arbitrary code running
// as the user — without OS enforcement it could read the encrypted vault and
// the master key straight off disk (or shell out to the keychain CLI). This
// wrapper closes that gap by launching the runner inside a per-platform
// sandbox that denies exactly those secret-bearing paths.
//
//   - macOS: sandbox-exec with a generated profile. allow-default (so node /
//     python and their deps keep working) plus a targeted deny on the vault
//     file, the key file, and exec of /usr/bin/security.
//   - Linux: bwrap (bubblewrap), masking the secret files with /dev/null.
//     Opt-in via FANTAZM_SKILL_SANDBOX=1 until validated on the target distro.
//
// When isolation can't be applied (tool missing, unsupported OS) the runner
// launches normally — the other layers (egress proxy, vault scoping, policy
// engine, stripped env) still apply.

// sandboxConfig is the per-Sandbox isolation state.
type sandboxConfig struct {
	enabled bool
	protect []string // absolute, canonicalized paths to deny the skill
}

// withProtectedPaths returns a copy of the config with isolation enabled for
// the given secret paths (vault file, key file). Paths are canonicalized so
// the sandbox literals match what the kernel sees (symlinks resolved).
func newSandboxConfig(protect ...string) sandboxConfig {
	var canon []string
	for _, p := range protect {
		if p == "" {
			continue
		}
		canon = append(canon, canonicalPath(p))
	}
	return sandboxConfig{enabled: len(canon) > 0, protect: canon}
}

// wrap turns the intended (bin, args) into the actual command to run, applying
// OS isolation when possible. ctx carries the timeout from the caller.
func (c sandboxConfig) wrap(ctx context.Context, bin string, args []string) *exec.Cmd {
	if !c.enabled || len(c.protect) == 0 {
		return exec.CommandContext(ctx, bin, args...)
	}
	switch runtime.GOOS {
	case "darwin":
		if path, err := exec.LookPath("sandbox-exec"); err == nil {
			full := append([]string{"-p", c.macProfile(), bin}, args...)
			return exec.CommandContext(ctx, path, full...)
		}
	case "linux":
		if brandenv.Get("FATHOM_SKILL_SANDBOX") != "1" {
			break // opt-in until validated on the target distro
		}
		if path, err := exec.LookPath("bwrap"); err == nil {
			bargs := c.bwrapArgs()
			bargs = append(bargs, bin)
			bargs = append(bargs, args...)
			return exec.CommandContext(ctx, path, bargs...)
		}
	}
	// No sandbox available — fall back to a plain launch. The deny is a
	// hardening layer, not the only boundary.
	return exec.CommandContext(ctx, bin, args...)
}

// macProfile renders a Seatbelt (SBPL) profile: allow everything by default so
// the language runtime and its deps work, then deny reads/writes of the secret
// files and deny launching the keychain CLI. Later rules win in SBPL, so the
// denies override the default allow for those specific paths.
//
// Residual: these are (literal …) rules, which match the exact path only. A
// same-uid skill could in principle hardlink/rename a secret file to a fresh
// path and read the copy (the deny doesn't follow the inode). We accept this
// for now because the high-value targets are mitigated in depth — the vault
// blob is encrypted, the master key normally lives in the keychain (no file),
// and the bwrap path on Linux masks the inode via a cross-FS bind. A
// (subpath …) deny on ~/.fantazm would be stronger but also blocks the
// skill-state dirs that live there (e.g. whatsapp-auth/), so it's not free.
func (c sandboxConfig) macProfile() string {
	var b strings.Builder
	b.WriteString("(version 1)(allow default)")
	b.WriteString("(deny file-read*")
	for _, p := range c.protect {
		b.WriteString(" (literal " + sbplString(p) + ")")
	}
	b.WriteString(")")
	b.WriteString("(deny file-write*")
	for _, p := range c.protect {
		b.WriteString(" (literal " + sbplString(p) + ")")
	}
	b.WriteString(")")
	// Block the easy keychain-read route (security CLI). Native SecKeychain
	// access would need a shipped native addon, a far higher bar.
	b.WriteString("(deny process-exec* (literal \"/usr/bin/security\"))")
	return b.String()
}

// bwrapArgs builds a bubblewrap invocation that mirrors the host filesystem
// read-only but masks each secret file with /dev/null, then drops into a fresh
// tmp. Kept minimal and conservative.
func (c sandboxConfig) bwrapArgs() []string {
	args := []string{
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
		"--unshare-ipc",
		"--die-with-parent",
	}
	for _, p := range c.protect {
		// Mask the file with an empty, unreadable bind so reads return nothing.
		args = append(args, "--bind", os.DevNull, p)
	}
	args = append(args, "--")
	return args
}

// sbplString quotes a path for an SBPL literal, escaping backslashes and
// double quotes.
func sbplString(p string) string {
	p = strings.ReplaceAll(p, `\`, `\\`)
	p = strings.ReplaceAll(p, `"`, `\"`)
	return `"` + p + `"`
}

// canonicalPath resolves symlinks so the sandbox literal matches the kernel's
// view (e.g. /tmp -> /private/tmp on macOS). The file itself may not exist yet
// (the vault is created on first write), so we resolve the parent dir and
// re-attach the base name.
func canonicalPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	dir := filepath.Dir(abs)
	if resolvedDir, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Join(resolvedDir, filepath.Base(abs))
	}
	return abs
}
