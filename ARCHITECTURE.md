# Fathom — Architecture

The README's diagram is marketing-shape ("here are the boxes"). This doc is
code-shape: which packages exist, why, what depends on what, where the
Node/Go boundary sits, and what's deliberately out of scope.

If you're trying to navigate the codebase or decide where new behaviour
belongs, start here.

## Package graph

```
                 cmd/fathom                ← single binary entrypoint
                      │
                      ▼
                internal/cli              ← cobra command tree
                      │
            ┌─────────┴──────────┬───────────────┐
            ▼                    ▼               ▼
   internal/agentfactory   internal/gateway   internal/scheduler
            │                    │
            ▼                    │
   ┌────────┴────────┐           │
   ▼                 ▼           ▼
internal/agent  internal/        internal/auth
   │ │           builtin               │
   │ │                                 │
   │ └──▶ internal/skills              │
   │            │                      │
   │            └──▶ internal/security (vault + egress proxy)
   │
   └──▶ internal/agent/llm
              │
              └─ HTTP only — no SDK deps

  internal/enterprise   (mounted onto gateway via extension hook;
                         loaded only in team/enterprise mode)
        │
        └──▶ internal/auth + internal/security (audit + sessions)

  internal/memory  internal/identity  ← sidecar clients
        │              │
        └─ MCP stdio   └─ HTTP (ZeroID)
           to recall      to a Sigil server

  pkg/types          ← the only package outside internal/. Holds the
                       cross-package types (Config, Mode, Session,
                       AuditEntry, SkillManifest). pkg/ instead of
                       internal/ because a future SDK package will
                       import these.
```

**Reading rule:** an arrow from A to B means "A imports B." There are no
cycles. `pkg/types` is at the bottom of every chain (used by everyone, imports
nothing internal).

## Subsystems — what each one does and why it exists

### `cmd/fathom`
Single-binary entrypoint. Just defers to `internal/cli`. Version is wired
via `-ldflags="-X main.Version=..."` by Goreleaser.

### `internal/cli`
Cobra command tree. Each subcommand (`chat`, `start`, `vault`, `schedule`,
`hub`, `init`) lives in its own file and self-registers via an `init()`
hook on the package-level `subcommands` slice. Keeps `root.go` small and
makes new commands additive.

### `pkg/types`
Pure data types. No logic, no imports beyond stdlib + yaml. Lives under
`pkg/` (not `internal/`) so a future SDK can import these without
re-defining them.

### `internal/config`
Loads `fathom.config.yaml` (or env-overridden values), merges with secure
defaults, validates and clamps obviously-wrong fields. Also loads
`fathom.policy.yaml` with deny-by-default fallbacks.

Why this is its own package: every other subsystem takes a `types.Config`
or `types.PolicyConfig`, so the loader has to come before everything else
in the dependency graph.

### `internal/security`
The biggest package. Holds **every** security primitive:

| File | Job |
|---|---|
| `crypto.go` | AES-256-GCM, scrypt key derivation, SHA-256 hash chain, token + ID generators. Byte-compatible with the TS vault format. |
| `audit.go` | Hash-chained tamper-evident log with FIFO bound + chain verification. |
| `sanitizer.go` | 23-pattern prompt-injection detector + zero-width unicode stripping. |
| `policy.go` | Deny-by-default rule engine with ** glob matching for path patterns. |
| `ratelimit.go` | Per-key token bucket. |
| `canary.go` | Honeypot token generation + scanning. Trips during egress if a memory secret ever appears in LLM output. |
| `secrets.go` | Encrypted file vault with atomic persist (`.tmp` + rename), per-skill scope check. |
| `vault_keyfile.go` | Auto-unlock keyfile at `~/.fantazm/.master.key` (0600). |
| `egress_proxy.go` | Localhost HTTP proxy that mediates outbound HTTPS for sandboxed skills. Injects credentials server-side. Enforces per-skill host allow-list + private-IP block-list (defeats DNS rebinding by checking the *resolved* address). |
| `mesh.go` | Convenience aggregator — one struct holding all of the above. |

Why one big package: these things share a lot of internal helpers (logger,
crypto primitives), and they're always used together. Splitting them would
create a "common-helpers package" that doesn't have a natural home.

### `internal/agent`
The agent runtime. Provider-agnostic.

| File | Job |
|---|---|
| `loop.go` | The agent loop: LLM call → tool calls → tool results → next iteration. Caps at 10 iterations by default. |
| `context.go` | Message-list assembly. Untrusted content gets wrapped in `<untrusted_data>`. Persona is appended to a non-removable baseline. |
| `tools.go` | Tool registry. Each tool's `Execute` is called with a `ToolContext` whose `GetSecret` is bound to the tool's owning skill name (so a "github" tool can't read a "gmail" secret). |
| `persona.go` | Loads `./.fantazm/persona.md` (project-local) or `~/.fantazm/persona.md` (user-global). |
| `json.go` | Tiny wrapper to keep encoding/json imports tidy. |

### `internal/agent/llm`
Provider implementations — `OpenAI`, `Anthropic`, `Ollama`. All hand-rolled
HTTP via `net/http`; no vendor SDKs. The `Provider` interface is one
method: `Chat(ctx, messages, tools) -> Response`. The agent loop is
provider-agnostic.

`Router` (router.go) holds a registry of named providers. The chat
session selects per-turn via `/use`; the `critique` tool selects a
different model for cross-model review. Legacy single-model config
(top-level `llm.provider`+`llm.model`) registers as one "default"
entry so existing setups keep working.

### `internal/auth`
Bearer-token auth (tokens stored as SHA-256 hashes) + session store. The
gateway uses this; the rest of the system doesn't touch it.

### `internal/gateway`
HTTP server. Routes `/api/v1/{health,message,session}` directly; falls
through to `Gateway.SetExtensionHandler` for unknown paths (used by
`internal/enterprise` to mount the admin API). One ticker per gateway
prunes expired sessions every 60s.

### `internal/scheduler`
Cron + SQLite persistent jobs, plus named routines.

- `cron.go` — 5-field parser plus aliases (`@hourly`, `@daily`, `@weekday`).
- `friendly.go` — turns plain-English `--every/--at` (e.g. `weekday` + `9am`,
  `30m`, `monday`) into a cron string the parser accepts. Raw cron still works.
- `store.go` — `modernc.org/sqlite` (pure Go, no CGO) with WAL. `jobs` carry an
  optional `skill` pin; `routines` are named, reusable prompts (+skill).
- `routines.go` — CRUD for named routines: run on demand (`fathom routine run`)
  or schedule by name (`fathom schedule add <name> --every …`), which snapshots
  the routine's prompt + skill into a job.
- `skill.go` — `SkillDirective` wraps a prompt so the agent uses just one skill.
  A soft instruction, not a hard tool lock — compatible with the single-prompt
  `Invoker` while keeping a scheduled routine focused.
- `scheduler.go` — Loop that ticks every 30s, fires due jobs through the
  agent. `inFlight` map guards against double-firing the same job when
  invokers outlast the tick. `markRun` anchors `next_run_at` at
  `time.Now()` (not the captured `runAt`) so slow invokers don't produce
  a catch-up loop.

### `internal/skills`
Subprocess sandbox + manifest registry + bridge.

- `manifest.go` — YAML parser with the same validation rules as the TS
  impl (lowercase-alphanumeric names, semver versions, METHOD URL network
  rules).
- `runner.js` + `runner_embed.go` — The Node skill runner is *embedded
  into the Go binary* via `//go:embed`. On first spawn, it's extracted
  to `~/.cache/fathom/runner.js`. Skills stay TypeScript; the host stays
  Go.
- `sandbox.go` — `os/exec` wrapper that spawns `node
  --disallow-code-generation-from-strings --frozen-intrinsics --no-deprecation
  runner.js`. Stdin/stdout JSON. Stripped env (only PATH, NODE_ENV, HOME,
  TMPDIR, optional `FANTAZM_PROXY_*`).
- `registry.go` — In-memory map of installed skills. `Invoke` finds the
  entry point (prefers `index.js`, falls back to `index.ts` for dev
  installs since Node 23.6+ strips types natively).
- `bridge.go` — `Bridge()` walks the skills directory, installs each,
  reads manifest-declared `tools`, and produces `ToolDefinition`s whose
  `Execute` pre-resolves vault secrets + issues a per-invocation egress
  proxy token + calls `Registry.Invoke`. Tool names are namespaced
  `<skill>_<tool>` to dodge cross-skill collisions.

### `internal/builtin`
Host-side `ToolDefinition`s registered by default. They don't go through
the subprocess sandbox because they're trusted host code, but they all
honor `ToolContext.Policy` — `web_search` calls `CheckNetwork`,
`read_file`/`write_file`/`list_files`/`edit_file`/`grep`/`glob` call
`CheckFilesystem`, `bash` calls `Evaluate({Action: "shell-exec"})`.

Toolkit:

- **Code work**: `bash`, `read_file`, `write_file`, `edit_file`
  (targeted unique-string replace), `list_files`, `grep` (RE2 regex,
  binary-file skip), `glob` (doublestar pattern).
- **Notes**: `create_note`, `search_notes`, `list_notes` (in-memory).
- **Web**: `web_search` (DuckDuckGo HTML-lite scrape).
- **Multi-model**: `critique` — auto-registered when ≥2 models are
  configured, lets the primary agent get a second opinion mid-turn.

File-editor's path guard uses `prefix + filepath.Separator` (not bare
`startsWith`) — the TS impl had a path-traversal bug there that we
don't re-introduce.

### `internal/agentfactory`
Composes everything: opens vault, builds mesh, picks LLM provider,
registers builtin tools, bridges installed skills, loads persona,
optionally starts egress proxy. Returns a `Result` with a `Handler`
function the gateway plugs into `SetMessageHandler`. The factory falls
back to an echo handler when no LLM API key is configured — gateway
still boots, health endpoint still works.

### `internal/enterprise`
RBAC + tenants + compliance exporter + SSO + admin HTTP routes.
**Loaded only in team/enterprise mode.** Personal-mode boots never
import this package's code. `enterprise.Mount(gateway, cfg, mesh)` is
a no-op when `cfg.Mode == personal`; in team/enterprise it constructs
the managers, bootstrap-assigns admin role to userId "admin" (or
`FANTAZM_BOOTSTRAP_ADMIN`), and attaches the admin handler to the
gateway's extension hook.

### `internal/hub`
Skill registry server — separate HTTP port (default 8791). On-disk
storage (`storage.go`) for tarballs + metadata. Serves
`/hub/v1/skills`, `/hub/v1/skills/{name}`, `/hub/v1/skills/{name}/download`,
`/hub/v1/stats`. Optional static frontend at `/` if a `hub-web/` dir is
provided.

### `internal/memory`
Client for the Fathom Recall sidecar (formerly agent-memory; the on-disk binary
is still `recall` until Fathom Recall renames internally). Spawns the binary as
an MCP stdio subprocess, frames JSON-RPC, exposes its 8 tools as Fathom
`ToolDefinition`s. Lifecycle is the gateway process. Failure to spawn
is non-fatal — memory is optional.

### `internal/identity`
HTTP client for the Sigil sidecar (formerly agent-identity; ZeroID-based).
Mints session tokens (`client_credentials`) and skill-delegated tokens
(RFC 8693 token-exchange) so every action is attributable, and supports
real-time `Revoke` for CAE-style cascade invalidation. Used in
team/enterprise mode to give every agent a stable WIMSE/SPIFFE URI.

## Node ↔ Go boundary

Skills are TypeScript subprocesses. The host is Go.

```
   ┌────────────────────────────┐
   │     fathom (Go binary)     │
   │                            │
   │   agent loop ────────────▶ │     spawn("node", runner.js)
   │   tool registry            │           │
   │   vault + egress proxy ───┘             ▼
   │                            ◀───── stdin/stdout JSON
   │   reads skill manifests          via the embedded runner
   └────────────────────────────┘
                                       ┌─────────────────────┐
                                       │ Node subprocess     │
                                       │  - reads stdin JSON │
                                       │  - dynamic-imports  │
                                       │    skill module     │
                                       │  - calls function   │
                                       │    with (input, ctx)│
                                       │  - writes stdout    │
                                       └─────────────────────┘
                                                │
                                                ▼ ctx.fetch
                                       ┌─────────────────────┐
                                       │ Egress proxy        │
                                       │ (back in Go,        │
                                       │  localhost HTTP)    │
                                       └─────────────────────┘
```

Why this split: keeping skills in TS preserves the JS ecosystem
(Anthropic SDK, OpenAI SDK, npm libraries). Putting the host in Go gives
us single-binary distribution + memory safety + better security
primitives. The cost is one extra runtime requirement on the user's
machine (Node 22+), called out in the install script.

## Mode tiers — how the package graph changes

```
personal mode:   cmd → cli → agentfactory → agent + gateway + scheduler
team mode:       all of the above + enterprise.Mount(gw) wires AdminAPI
                   onto Gateway.SetExtensionHandler
enterprise mode: team + enterprise.SSOManager configured from cfg.Enterprise.SSO
```

The `internal/enterprise` package is **imported** unconditionally (we link
it), but `Mount()` is a no-op when `cfg.Mode == personal`, so the
managers + routes don't get created. If you want to physically exclude
the enterprise binary code, you'd use Go build tags — out of scope for
v0.1, easy to add later.

## What's deliberately out of scope

- **WebSocket endpoint.** The TS impl had one; the Go gateway doesn't yet.
  REST + scheduler + admin work; WS is a half-day add when we need it.
- **`fathom install` CLI.** The hub server exists; the CLI that pulls a
  skill from the hub + walks the OAuth flow doesn't. The TS implementation
  is the reference for the OAuth handling.
- **Container-per-session sandbox.** The subprocess sandbox is the
  isolation tier for v0.1. Docker / Apple Container backends are roadmap.
- **Self-modifying skills** (Voyager-style). Currently the agent can't
  author + register new skills at runtime. Static analysis on
  agent-authored code is the gate.
- **Memory-tier consolidation inside Fathom.** Fathom Recall does this; Fathom
  treats it as a black-box sidecar. If Fathom Recall goes down, Fathom keeps
  working without long-term memory.

## Where new code belongs

Decision tree for "where do I put this":

- **It's a new tool the LLM can call.** → in-process: `internal/builtin/`.
  Subprocess: a skill under `/skills/SKILLNAME/` with `skill.manifest.yaml`.
- **It's a new security check.** → `internal/security/`.
- **It's a new CLI command.** → `internal/cli/NAME.go`, append to
  `subcommands` in `init()`.
- **It's a new HTTP route on the gateway.** → if it's enterprise-shaped,
  wire it via `enterprise.AdminAPI.Handle`. If it's core, add a route
  in `internal/gateway/gateway.go`.
- **It's a new LLM provider.** → `internal/agent/llm/PROVIDER.go` +
  one new case in `internal/agent/llm/provider.go:New()`.
- **It's a new persistent type.** → `pkg/types/` for the struct + a
  loader/store under the relevant `internal/` package.

If the answer isn't obvious, it probably means we don't have the right
package yet — file an issue.
