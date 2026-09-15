# Contributing to Fathom

Thanks for being here. This doc covers the practical bits: how to set up,
how to test, what to expect from code review. The high-level shape lives
in [`ARCHITECTURE.md`](./ARCHITECTURE.md) — read that first if you're
new to the codebase.

## Setup

Requirements:

- **Go 1.23+** — `brew install go` on macOS, your distro's package
  manager on Linux. Goreleaser binaries on Windows.
- **Node 22+** — only if you want to run / test installed skills locally.
  The core agent and tests don't need it; skill subprocesses do.
- **git** — obviously.

Clone and build:

```bash
git clone https://github.com/zdaniels/fathom.git
cd fathom
go mod download
go build ./cmd/fathom
./fathom --version
```

## Day-to-day

```bash
# Build everything
go build ./...

# Run tests
go test ./...

# Lint
go vet ./...

# Format (gofmt is also fine; goimports also tidies imports)
gofmt -w .

# Run the local binary against a temp data dir
FANTAZM_VAULT_PATH=/tmp/v FANTAZM_VAULT_KEY=/tmp/k \
FANTAZM_SCHEDULE_DB=/tmp/s.db \
./fathom chat
```

## Where things live

- `cmd/fathom/main.go` — entrypoint. Single file.
- `internal/<subsystem>/` — most of the code. See `ARCHITECTURE.md`
  for the package graph and "where new code belongs" decision tree.
- `pkg/types/` — cross-package public types.
- `runner/` + `internal/skills/runner.js` — the embedded Node skill
  runner. `runner_embed.go` uses `//go:embed` to bundle it.
- `skills/<name>/` — TypeScript skill source + manifests. Independent
  of the Go host.

## Code style

- **Format with `gofmt`.** No exceptions. `go vet` must pass.
- **Comments answer "why," not "what."** A future reader knows what
  `x := y + 1` does; they may not know why we anchor `markRun` at
  `time.Now()` instead of the captured `runAt` (we do because slow
  invokers were otherwise producing a catch-up loop). Comments should
  capture that kind of context.
- **Doc-comment public functions and types.** `go doc` should produce
  something useful.
- **Don't add error handling for cases that can't happen.** If a
  function inside our own code can't fail, don't wrap it in
  defensive checks — that's noise. Only validate at trust boundaries
  (user input, external APIs, file I/O).
- **No vendored frameworks.** stdlib + a handful of focused libraries
  (`cobra`, `nhooyr.io/websocket`, `modernc.org/sqlite`, `yaml.v3`).
  If you're tempted to add a new dependency, ask first.

## Commit messages

Two-tier — subject line and a body that explains the why.

- **Subject:** present tense, imperative, ≤ 70 chars.
  *"Fix scheduler markRun catch-up loop"*, not
  *"fixed the scheduler"*.
- **Body:** what changed and **why**. Reference the failure mode
  the change addresses, not just the line edit. If you're touching
  load-bearing code (security mesh, agent loop, scheduler), include
  what you verified end-to-end.
- Close with `Co-Authored-By:` lines for any contributors.

Multi-component changes: one logical change per commit when reasonable.
A "rename + reorganise + bugfix" commit is harder to review and
review than three smaller ones.

## Change cadence

Prefer small, focused follow-up PRs: one concrete fix or usable feature increment
per PR, with its tests and documentation. Use logical commits that a reviewer can
understand independently. Keep the main branch usable between increments.

## Pull request checklist

Before opening a PR:

- [ ] `go build ./...` clean
- [ ] `go vet ./...` clean
- [ ] `go test ./...` passes
- [ ] If you touched the security mesh, vault, egress proxy, or
      scheduler: add a test or document why one doesn't apply
- [ ] If you added a new CLI command or env var: README updated
- [ ] If you added a new subsystem: `ARCHITECTURE.md` updated
- [ ] If you changed the wire shape of anything skills depend on
      (manifest schema, runner protocol, egress attach specs):
      `CHANGELOG.md` updated and at least one TS skill verified to
      still work

## Code review

We're optimising for **catching real bugs** and **keeping the
codebase legible to future contributors**. Reviewers focus on:

1. **Correctness on the failure paths.** Happy-path code review is
   table stakes; what we care about is "what happens when this hits
   an error / runs out of memory / loses the network / gets a
   malformed input."
2. **Security-sensitive code gets extra scrutiny.** Anything in
   `internal/security/` or the skill sandbox needs a reviewer who's
   thought about adversarial inputs.
3. **Architecture coherence.** New code that doesn't fit the
   package graph — a `internal/util/` that ends up importing
   everything, a tool registry that bypasses the policy engine —
   triggers a "where does this belong" discussion before the diff
   review.
4. **Comments where the why isn't obvious.** Lack of context is the
   #1 reason future-you (or your replacement) misreads load-bearing
   code.

Reviewers will run the code, not just read it. Be prepared for "I
tried this and got X" responses.

## Reporting bugs

Open an issue with:

- What you ran (full command + relevant config)
- What you expected
- What actually happened (logs, error messages — redact secrets)
- Your environment: OS, Go version, Node version if applicable

For security-sensitive issues (vault escape, audit chain integrity,
sandbox escape, prompt-injection bypass): **don't open a public
issue**. Use [private vulnerability reporting](https://github.com/zdaniels/fathom/security/advisories/new) instead.

## Roadmap awareness

What's actively being worked on lives in
[`CHANGELOG.md`](./CHANGELOG.md). What's NOT being worked on (and
why) is in `ARCHITECTURE.md` under "What's deliberately out of
scope." If you want to work on something that's listed as out of
scope, file an issue and let's discuss before you write the PR.

## Licensing

All Fathom project code, including team and enterprise features, is licensed
under [MIT](LICENSE.md). Contributions are accepted under the same license.
Keep third-party copyright and license notices intact.
