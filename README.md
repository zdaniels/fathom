<p align="center">
  <img src="assets/logo/fathom-logo-400.png" alt="Fathom" width="240" />
</p>

<h1 align="center">Fathom</h1>

<p align="center"><strong>Your AI agent. On your laptop. With teeth.</strong></p>

<p align="center">
  <a href="https://github.com/zdaniels/fathom/releases/latest"><img src="https://img.shields.io/github/v/release/zdaniels/fathom?color=cobalt" alt="release"></a>
  <a href="https://github.com/zdaniels/fathom/actions/workflows/ci.yml"><img src="https://github.com/zdaniels/fathom/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/zdaniels/fathom/stargazers"><img src="https://img.shields.io/github/stars/zdaniels/fathom?style=social" alt="stars"></a>
  <a href="./LICENSE.md"><img src="https://img.shields.io/github/license/zdaniels/fathom" alt="license"></a>
</p>

<p align="center">
  <a href="https://github.com/zdaniels/fathom#demo"><strong>See the demo →</strong></a>
</p>

---

Self-hosted AI agent. Single Go binary. Multi-LLM — Claude, GPT, Gemini, Bedrock, DeepSeek, Grok, MiMo, Groq, OpenRouter, Together, Fireworks, Mistral, HuggingFace, plus local (Ollama, LM Studio). Encrypted vault, sandboxed skills, hash-chained audit. No accounts, no cloud, no telemetry.

## Install from source

```bash
git clone https://github.com/zdaniels/fathom.git
cd fathom
go build -o fathom ./cmd/fathom
./fathom init
./fathom
```

Requires Go 1.25 or newer. Install [Recall](https://github.com/zdaniels/recall)
for shared local memory. The agent works independently when Recall is absent.

See [COMPATIBILITY.md](COMPATIBILITY.md) for supported configuration paths
and environment variables.

## Two binaries, same codebase

`fathom` is the agent — what you run on your laptop. **`fathom-gateway`** is a separate binary built from the same repo: a standalone, multi-tenant, OpenAI-compatible LLM proxy you drop into a VPC. Same provider registry, same vault, same hash-chained audit log — without the agent loop, skills, or scheduler.

```bash
go build -o fathom-gateway ./cmd/fathom-gateway
fathom-gateway --config /etc/fathom-gateway/config.yaml
```

What it does:
- One endpoint, 14 LLM providers. Apps point their OpenAI SDK at the gateway; the gateway routes per request to whichever upstream you've configured.
- Per-team bearer tokens, model allow-lists (globs), token + RPM quotas.
- Vendor keys live in the gateway's vault — never leave the VPC.
- Hash-chained audit log of every request, every denial, every quota hit.

Reach for it when:
- Your company already self-hosts Fathom for compliance and you want every internal app to route through the same vetted LLM surface.
- You're under HIPAA / PCI / FedRAMP / EU AI Act and can't have a third-party hop (OpenRouter, Helicone, etc.) in the data path.
- You've negotiated separate contracts with Azure OpenAI + Bedrock + an internal GPU cluster and need one unified API across them.

Sample config: [`examples/fathom-gateway.config.yaml`](./examples/fathom-gateway.config.yaml). v0.1 supports OpenAI-protocol upstreams (covers 12 of the 14 providers Fathom integrates); Anthropic-native and Bedrock-native request translation lands in v0.2.

## Why it exists

Most agent platforms either ship your data to a hosted service or require Kubernetes to stand up. Fathom does neither. The agent loop, security mesh, vault, scheduler, and gateway are one Go binary. Audit log is hash-chained — tamper one entry and the chain breaks visibly.

## How it compares

|                                  | Fathom | [Plandex][p] | [Ollama][o] | [Claude Code][cc] |
|----------------------------------|:------:|:-----:|:------:|:-----------:|
| Self-hosted                       | ✅      | ✅     | ✅      | ❌           |
| Open source                       | ✅      | ✅     | ✅      | ❌           |
| Multi-LLM (14 providers — hosted + local) | ✅ | partial | local | Claude only |
| Phone pairs to your own backend   | ✅      | ❌     | ❌      | ❌¹          |
| Sandboxed third-party plugins (TS + Py) | ✅ | builtin only | n/a | builtin only |
| Policy engine + hash-chained audit log + vault | ✅ | ❌ | ❌ | ❌ |
| Static single binary              | ✅      | ✅     | ✅      | Node CLI    |

<sub>¹ Claude Code ships a mobile app, but it connects to Anthropic's hosted Claude — not a backend you control. Fathom pairs your phone to a gateway running on your laptop with your keys.</sub>

[p]: https://github.com/plandex-ai/plandex
[o]: https://github.com/ollama/ollama
[cc]: https://docs.claude.com/en/docs/agents-and-tools/claude-code/overview

## What it can do today

```text
# In `fathom chat`:
> find all uses of getUserById in this repo                  ← grep tool
> add error handling to the function at main.go:42           ← read_file + edit_file
> run the tests and tell me what's failing                   ← bash tool (policy-gated)
> search the web for the latest changes to the EU AI Act     ← web_search tool

# Multi-model: switch mid-conversation
> /use coding                ← next turn goes to Claude
> rewrite this function...
> /review                    ← runs that answer through GPT-4o for critique

# Scheduled work (runs in the background while `fathom start` is up):
$ fathom schedule add --every weekday --at 9am "summarize my open PRs"
$ fathom schedule add --every 30m "check for new urgent email"

# Reusable routines — run on demand or put on a clock by name:
$ fathom routine add inbox --skill gmail "summarize anything unread"
$ fathom routine run inbox
$ fathom schedule add inbox --every weekday --at 8am

$ fathom start
```

Out-of-the-box tools: `bash`, `read_file`, `write_file`, `edit_file`, `list_files`, `grep`, `glob`, `web_search`, `create_note`, `search_notes`, `list_notes`. With the [Fathom Recall](https://github.com/zdaniels/recall) memory sidecar installed: 8 more recall tools for durable cross-session memory.

## Architecture

```
┌──────────────────────────────────────────────────────────────┐
│                         FATHOM                             │
│                                                             │
│  ┌──────────┐   ┌──────────┐   ┌────────────────────────┐  │
│  │ Channels │──▶│ Gateway  │──▶│   Security Mesh         │  │
│  │  CLI     │   │ Auth     │   │   Sanitizer             │  │
│  │  WebSock │   │ Sessions │   │   Policy engine         │  │
│  │  REST    │   │ Router   │   │   Audit log (hashed)    │  │
│  └──────────┘   └──────────┘   │   Rate limiter          │  │
│                                │   Secrets vault         │  │
│                                │   Canary tokens         │  │
│                                │   Egress proxy          │  │
│                                └───────────┬─────────────┘  │
│                                            ▼                │
│                              ┌──────────────────────────┐   │
│                              │      Agent Runtime       │   │
│                              │   Context + persona      │   │
│                              │   LLM (OpenAI/Anthropic/ │   │
│                              │        Ollama)           │   │
│                              │   Tool registry          │   │
│                              └────┬─────────────────┬───┘   │
│                                   ▼                 ▼       │
│                            ┌────────────┐   ┌──────────────┐│
│                            │  Skills    │   │  Scheduler   ││
│                            │ (subproc + │   │ (SQLite + cron)│
│                            │  egress    │   └──────────────┘│
│                            │  proxy)    │                   │
│                            └────────────┘                   │
└──────────────────────────────────────────────────────────────┘
```

## Making it yours: the fork path

Fathom is structured so that 95% of customization happens through config + persona + skills — no source edits, no fork to maintain. But for the cases where you genuinely want to own the code (private agent for a team, custom UI strings, vendored integrations), forking is supported and tracked here.

### What to change, and how

| You want to customize | Recommended approach |
|---|---|
| Agent name, voice, instructions | `.fantazm/persona.md` (project) or `~/.fantazm/persona.md` (global) |
| System prompt baseline (security rules) | `internal/agent/context.go` — **fork only**, since the baseline is intentionally not user-overridable |
| LLM provider or model | `fathom.config.yaml` → `llm:` block (multi-model via `llm.models:`) |
| Available tools | Add a skill in `/skills/` with a `skill.manifest.yaml` — see [Skills](#skills) below |
| Mobile UI strings / colors | `internal/gateway/web/{index.html,chat.css}` — fork; rebuild the binary |
| Menubar app | `apps/macos/Sources/Fathom/*.swift` — fork; rebuild the .app |
| Boot banner / CLI copy | `internal/cli/ui/theme.go` + the various `cli/*.go` welcome strings — fork |
| Routing logic (intent classifier) | `internal/agent/router/` — fork |
| What "minimal" profile keeps vs strips | `internal/agentfactory/factory.go` — search for `minimal` — fork |

### Worked example: ship a private agent for your team

Imagine you want "Acme Agent" — same Fathom guts, but with your team's branding, a vendored Salesforce skill that isn't open source, and a stricter default policy.

```bash
# 1. Fork on GitHub, then clone your fork.
git clone https://github.com/your-org/acme-agent.git
cd acme-agent

# 2. Brand surfaces you want to swap (just text edits, no logic change):
#    - apps/macos/Sources/Fathom/RootView.swift   → "Acme" instead of "Fathom"
#    - internal/cli/ui/theme.go                   → wordmark + welcome text
#    - internal/gateway/web/index.html            → page title, brand image
#    - assets/                                    → drop in your logo PNGs

# 3. Vendor your private Salesforce skill:
mkdir -p skills/salesforce
# ...add skill.manifest.yaml + the skill's index.ts...

# 4. Tighten the default policy:
#    - internal/cli/init.go                       → change the "defaults:" block
#                                                    written by `fathom init`

# 5. Build + distribute.
go build -o acme ./cmd/fathom
# Ship `acme` to your team. Same single-binary distribution as upstream.
```

### Keeping current with upstream

Forks are yours to maintain. To pick up upstream fixes and features:

```bash
git remote add upstream https://github.com/zdaniels/fathom.git
git fetch upstream
git rebase upstream/main      # or merge, your preference
```

If you mostly touched surfaces in the customization table above, conflicts will be rare — Fathom's internals are stable enough that the brand layer + skills don't move underfoot. The biggest churn risk is in `internal/agent/` (loop + context) and `internal/agentfactory/` — try not to fork those unless you need to.

### When to NOT fork

- You want a different LLM → config change, no fork needed
- You want to disable scheduler/threads/audit → `profile: minimal` in config
- You want SSO → `--enterprise` at init time
- You want a private skill → add it to `/skills/`, no fork needed

The recipe: fork last, config first.

## Personalize it

Edit `.fantazm/persona.md` in your project (or `~/.fantazm/persona.md` globally) to give your agent a name, voice, and instructions. The file is **appended** to a non-removable security baseline — your persona can shape behaviour but can't disable the security rules.

```markdown
# Andy

You are Andy. Terse and direct.
When you write code, default to Go and prefer named exports.
Always confirm before doing anything destructive.
```

## Skills

Skills are scoped tools your agent can use — each declares the permissions it needs (network domains, filesystem scope, secrets). Skills live under `/skills/` in your project directory; Fathom auto-detects and bridges them at startup.

Skill manifests in `data/hub/metadata/` ship with the repo for reference: calendar, discord, github, gmail, slack, telegram. The Fathom Secure Hub for one-command skill install is on the 1.0 roadmap.

Built-in starter tools (no setup): **bash**, **read_file**, **write_file**, **edit_file**, **list_files**, **grep**, **glob**, **web_search**, **create_note / search_notes / list_notes**.

Skills run in a **Node subprocess** with:
- `--disallow-code-generation-from-strings` (no `eval`)
- `--frozen-intrinsics` (no prototype tampering)
- Parent `process.env` stripped (skill can't read your shell secrets)
- An **egress proxy token** — the skill can only make outbound HTTPS to its manifest-declared hosts, and credentials are injected by the proxy, never held by the skill itself

## Security model (honest version)

What's true *today*:

- **Hash-chained audit log** — every tool call, LLM request, and policy decision is logged with a SHA-256 chain. Tampering breaks the chain. Verifiable. ✅
- **Prompt-injection sanitizer** — 23 patterns for known attack shapes; tagged `<untrusted_data>` separation in the LLM context. ✅
- **Policy engine** — deny-by-default rules from `fathom.policy.yaml`, evaluated per-tool. ✅
- **Canary tokens** — honeypot strings injected into context; if the LLM ever emits one, the session is flagged. ✅
- **Rate limiting** — token-bucket per user. ✅
- **Encrypted secrets vault** — AES-256-GCM file at `~/.fantazm/vault`, auto-unlocked from a 0600 keyfile at `~/.fantazm/.master.key` (SSH-agent style). OAuth tokens + LLM keys live here, scoped per-skill. ✅
- **Subprocess-isolated skills** — Node subprocess per invocation, stripped env, hardening flags. ✅
- **Egress proxy** — sandboxed skills route outbound HTTPS through a localhost proxy that resolves credentials from the vault and attaches them server-side (bearer / header / google-oauth refresh). The skill describes the *shape* of the auth it needs and never sees the value. Includes a hard IP block-list (RFC1918, loopback, link-local incl. AWS/GCP metadata `169.254.169.254`) checked against the *resolved* address — defeats DNS rebinding. ✅
- **Container-per-session sandbox** is the next hardening tier (Phase 3 of the roadmap). The current subprocess boundary is a real process boundary but not a filesystem/network capability sandbox.

## Threads — conversations that follow you

Every chat is a persistent thread (`~/.fantazm/threads.db`). Open one on your Mac menubar, continue it on your phone, finish it from the terminal — same history, live-updating across devices via SSE.

```bash
fathom join                # pick from your threads in the terminal
fathom join --new          # start a fresh one
fathom join --list         # list threads + exit
```

Auto-title kicks in after the first message: takes the first sentence (stripping code fences, capping at 60 chars). No extra LLM call.

Deleting a thread is soft — the row stays around (still in SQLite) but is hidden. A 6-second undo toast lets you recover; tap **Undo** and it's back. After the toast, the thread stays soft-deleted until a sweeper hard-purges it (not implemented yet — manually drop the row if you really want it gone).

## Pair devices, survive restarts

`fathom pair` mints a 6-digit code; the phone (or any browser) scans the QR and gets a long-lived device token bound to that Fathom instance. Tokens are SHA-256 hashed at rest in `~/.fantazm/devices.db` — **paired devices survive `fathom service restart`**. Manage with:

```bash
fathom devices list             # everything that's paired + last-seen time
fathom devices revoke <id>      # kill a specific device immediately
```

Reach the gateway from outside your LAN via either:

- **Tailscale** (recommended) — `tailscale up` on both devices; `fathom pair` auto-detects the tailnet IP. WireGuard transit, no public surface.
- **Cloudflare Tunnel** — `fathom start --tunnel` spawns `cloudflared` and prints a `*.trycloudflare.com` URL. Refuses to expose the bootstrap admin token; pair at least one device first.

## Modes — one product, three felt experiences

Set `mode:` in `fathom.config.yaml` (or `FANTAZM_MODE` env, or `fathom init --mode=…`):

| Mode | Default? | Mounts | When you want it |
| ---- | -------- | ------ | ---------------- |
| `personal` | ✓ | gateway + chat + skills + scheduler + vault | One user, one laptop. |
| `team` |   | personal + multitenancy + RBAC + admin API (`/api/v1/admin/*`) | A few teammates sharing a self-hosted instance. |
| `enterprise` |   | team + SSO/OIDC + compliance exports (SOC 2 / HIPAA / GDPR) | Regulated environments. |

Personal mode never loads the enterprise package at runtime — single-user installs stay lean.

## CLI

```
fathom init [--mode=...]         Setup wizard
fathom chat                      Local chat REPL with the agent
fathom start [config]            Start the gateway (HTTP + scheduler + admin in team/enterprise)
fathom vault list                Show secrets in the vault (names + scopes)
fathom vault set NAME            Store a secret (prompts for value)
fathom vault get NAME            Print a secret
fathom vault import-env          Migrate known keys from .env into the vault
fathom routine add NAME PROMPT   Save a reusable routine (--skill to pin one skill)
fathom routine run NAME          Run a routine once, now
fathom routine list              List saved routines
fathom schedule add WHEN TASK    Schedule a prompt or routine (--every/--at or cron)
fathom schedule list             List scheduled jobs
fathom schedule run ID           Force-run a scheduled job
```

## Use Fathom from your phone

The gateway ships an installable PWA at `/`. Pair a phone in three steps:

```bash
# 1. In one terminal:
fathom start

# 2. In another terminal:
fathom pair
#   → prints a QR code + 6-digit code, blocks until claimed

# 3. On your phone: scan the QR (or visit the URL and enter the code).
#    Tap "Add to home screen" for a native-feeling app.
```

The QR auto-detects the right URL for your phone in this priority order:

1. **Cloudflare Tunnel** — `fathom start --tunnel` spawns an anonymous
   `*.trycloudflare.com` URL. Requires `cloudflared` in PATH
   (`brew install cloudflared`). Works from anywhere with internet.
2. **Tailscale** — if you have Tailscale on both devices, the QR uses
   your laptop's Tailscale IP. Works through NAT, no public surface.
3. **LAN** — same-wifi pairing. Uses the laptop's local IP.
4. **localhost** — only useful when testing on the laptop itself.

Override the host explicitly with `--host`:

```bash
fathom pair --host https://my-tunnel.example.com
fathom pair --host http://100.x.y.z:8790
```

Manage paired devices:

```bash
fathom devices list             # see what's paired + last-seen times
fathom devices revoke <id>      # kill a device's token immediately
```

**Security model**: every device gets its own SHA-256-hashed bearer
token, separate from the bootstrap admin token. Pairing codes are
single-use, 60-second TTL, rate-limited 5/min/IP — brute force on a
6-digit code is statistically negligible in that window. `--tunnel`
refuses to start if the only token would be the bootstrap admin (too
risky over a public URL); pair at least one device first.

## HTTP API (when `fathom start` is running)

```
POST   /api/v1/message            Send a message (Bearer token auth)
POST   /api/v1/stream             Same, but Server-Sent Events for streaming UI
GET    /api/v1/session            Active sessions for the caller
GET    /api/v1/health             Liveness check (no auth)

POST   /api/v1/pair/start         Mint a 6-digit pairing code (auth required)
GET    /api/v1/pair/watch?code=…  Long-poll until the code is claimed (auth)
POST   /api/v1/pair/claim         Redeem a code for a device token (no auth, IP-rate-limited)
GET    /api/v1/devices            List paired devices
DELETE /api/v1/devices/{id}       Revoke a paired device
```

Team/enterprise mode also mounts:
```
GET    /api/v1/audit              Hash-chained audit log (RBAC-gated)
GET    /api/v1/admin/users        List users + roles
POST   /api/v1/admin/users/role   Assign role
GET    /api/v1/admin/tenants      List tenants
POST   /api/v1/admin/tenants      Create tenant
POST   /api/v1/admin/compliance   Generate SOC2 / HIPAA / GDPR report (json | csv)
```

## Configuration

```yaml
# fathom.config.yaml — minimal single-model setup
mode: personal
host: 127.0.0.1
port: 8790
llm:
  provider: anthropic
  model: claude-opus-4-8
dataDir: ./data
policyFile: ./fathom.policy.yaml
```

Or register many models and switch between them at runtime with
`/model <name>` (Mac menubar, mobile, or CLI). Entries whose API key
is missing are skipped at startup, so it's fine to list every provider
and only enable the ones you have keys for — see [LLM providers](#llm-providers)
for the full provider/env-var table.

```yaml
llm:
  default: hermes            # name of the entry used when no /model pin
  models:
    # local (no API key needed)
    hermes:      { provider: ollama,  model: hermes3:8b }
    qwen:        { provider: ollama,  model: qwen3:14b }
    qwen-large:  { provider: ollama,  model: qwen3:32b }
    gemma:       { provider: ollama,  model: gemma4:e4b }
    gemma-e2b:   { provider: ollama,  model: gemma4:e2b }   # smallest/fastest, ~3GB VRAM
    gpt-oss:     { provider: ollama,  model: gpt-oss:20b }
    lmstudio:    { provider: lmstudio, model: local-model }

    # hosted (need the matching key in the vault or env)
    opus:        { provider: anthropic, model: claude-opus-4-8 }
    sonnet:      { provider: anthropic, model: claude-sonnet-4-6 }
    haiku:       { provider: anthropic, model: claude-haiku-4-5 }
    gpt5:        { provider: openai,    model: gpt-5.5 }
    gemini:      { provider: gemini,    model: gemini-2.5-pro }
    deepseek:    { provider: deepseek,  model: deepseek-v4-pro }
    deepseek-flash: { provider: deepseek, model: deepseek-v4-flash }
    grok:        { provider: xai,       model: grok-4 }
    groq-llama:  { provider: groq,      model: llama-3.3-70b-versatile }
    mistral:     { provider: mistral,   model: mistral-large-latest }
```

```yaml
# fathom.policy.yaml
version: 1
defaults: { network: deny, filesystem: read-only, shell: deny, secrets: isolated }
rules: []
```

Env overrides: `FANTAZM_MODE`, `FANTAZM_HOST`, `FANTAZM_PORT`, `FANTAZM_DATA_DIR`, `FANTAZM_LOG_LEVEL`, `FANTAZM_VAULT_PATH`, `FANTAZM_VAULT_KEY`, `FANTAZM_SCHEDULE_DB`, `FANTAZM_SKILLS_DIR`.

### Intent-based routing (optional)

For local LLMs that get sluggish when given 30+ tools at once, opt into the **routing layer**: a tiny fast classifier picks one of N "profiles" per message, and the chosen profile decides which model handles it AND which subset of skills the model sees as tools. Prompt context per request shrinks from "all 38 tools" to "just the 5 relevant ones."

```yaml
llm:
  default: local
  models:
    classifier: { provider: ollama, model: qwen2.5:0.5b }   # tiny + fast
    local:      { provider: ollama, model: hermes3:8b }
    coder:      { provider: ollama, model: qwen3:32b }

routing:
  router:
    model: classifier
    fallback: general          # used when classification fails
  profiles:
    general:
      model: local
      categories: [memory, productivity]
    communication:
      model: local
      skills: [gmail, slack, whatsapp, telegram]
      systemPrompt: |
        You are concise. Confirm with the user before sending anything.
    code:
      model: coder
      categories: [development]
      skills: [file-editor, bash, grep]
```

`skills:` is an exact-name whitelist; `categories:` matches each skill's `category` field (from its `skill.manifest.yaml`). They union — combine for "this category PLUS these specific extras." Omit `routing:` entirely to use the legacy single-agent path where every model sees every tool.

### External egress proxy (Charon) + skill sandbox (Chasm) + telemetry (Beacon)

Fathom can offload its credential-injecting egress proxy, skill sandbox, and observability layer to standalone services. All three are part of the github.com/zdaniels/fathom modular agent stack — same brand, same security posture, swappable independently.

```yaml
# fathom.config.yaml
egress:
  proxy: http://127.0.0.1:8889       # Charon — github.com/fantazmai/charon
  token_file: ~/.charon/token

skills:
  runtime: chasm                      # Chasm — github.com/fantazmai/chasm
  chasm_url: http://127.0.0.1:8890
  chasm_token_file: ~/.chasm/token
  default_image: ghcr.io/fantazmai/chasm-node22:latest

telemetry:
  beacon_url: http://127.0.0.1:4318   # Beacon — github.com/fantazmai/beacon
  beacon_token_file: ~/.beacon/token
  service_name: fathom
```

What each one does:

- **Charon** replaces Fathom's in-process egress proxy. Agents make outbound calls *as themselves*; Charon looks up the right credential per its policy, injects it at proxy-time, audit-logs the call. Credentials never enter the agent's context.
- **Chasm** replaces Fathom's Node-subprocess skill sandbox. Each skill invocation runs in a transient hardened container (read-only rootfs, all caps dropped, memory/CPU/PID limits, no network unless via Charon).
- **Beacon** receives OTLP/HTTP/JSON spans. Every agent iteration becomes a root span, every LLM call and tool invocation becomes a child — searchable by trace ID, replayable with `beacon trace <id>`, tamper-evident on disk. Any OTLP-compatible receiver works (Honeycomb, Datadog via the OTel Collector, etc.) — Beacon is just the lightweight reference implementation.

All three are optional. Any combination works. The legacy in-process implementations stay as the defaults so existing configs don't break.

## Building from source

```bash
git clone https://github.com/zdaniels/fathom.git
cd fathom
go build -o fathom ./cmd/fathom
./fathom --version
```

Node 22+ must be installed for skills to run (they're TypeScript subprocesses). The core agent works without Node, but skill installation requires it.

## Docker

```bash
docker compose up -d
```

Hardened image (non-root user, read-only data volume), with Node bundled for skill execution.

## Kubernetes

```bash
helm install fathom ./helm --set mode=enterprise --set auth.mode=oidc
```

## LLM providers

Set `provider:` + `model:` in any `llm.models` entry. Hosted providers
read their key from the vault or environment (the var in the last
column); entries whose key is missing are skipped at startup, so you
can list everything and only the configured ones register.

| Provider     | `provider:` | Example `model:`          | Auth env var(s)                                            | Local |
| ------------ | ----------- | ------------------------- | ---------------------------------------------------------- | ----- |
| Anthropic    | `anthropic` | `claude-opus-4-8`         | `ANTHROPIC_API_KEY`                                        | No    |
| OpenAI       | `openai`    | `gpt-5.5`                 | `OPENAI_API_KEY`                                           | No    |
| Google       | `gemini`    | `gemini-2.5-pro`          | `GEMINI_API_KEY`                                           | No    |
| AWS Bedrock  | `bedrock`   | `anthropic.claude-opus-4-8` | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN` (optional) | No |
| DeepSeek     | `deepseek`  | `deepseek-v4-pro`         | `DEEPSEEK_API_KEY`                                         | No    |
| xAI (Grok)   | `xai`       | `grok-4`                  | `XAI_API_KEY`                                              | No    |
| Xiaomi MiMo  | `xiaomi`    | `mimo-7b-rl`              | `XIAOMI_MIMO_API_KEY`                                      | No    |
| Groq         | `groq`      | `llama-3.3-70b-versatile` | `GROQ_API_KEY`                                             | No    |
| OpenRouter   | `openrouter`| `anthropic/claude-opus-4-8` | `OPENROUTER_API_KEY`                                     | No    |
| Together     | `together`  | `meta-llama/Llama-3.3-70B`| `TOGETHER_API_KEY`                                         | No    |
| Fireworks    | `fireworks` | `accounts/fireworks/models/deepseek-v4` | `FIREWORKS_API_KEY`                          | No    |
| Mistral      | `mistral`   | `mistral-large-latest`    | `MISTRAL_API_KEY`                                          | No    |
| HuggingFace  | `huggingface` | `deepseek-ai/DeepSeek-R1` | `HF_TOKEN`                                              | No    |
| Ollama       | `ollama`    | `qwen3:14b`               | — (defaults to `http://localhost:11434`)                  | Yes   |
| LM Studio    | `lmstudio`  | `local-model`             | — (defaults to `http://localhost:1234/v1`)                | Yes   |

DeepSeek, xAI, Xiaomi, Groq, OpenRouter, Together, Fireworks, Mistral,
HuggingFace, and LM Studio all speak the OpenAI chat-completions wire
format — each just points the OpenAI client at a different base URL.
Override `baseUrl:` on any entry to hit a proxy, a self-hosted vLLM
endpoint, or a region-specific host. Switch with one config line. No
vendor lock-in.

**HuggingFace** (`huggingface`, alias `hf`) targets the [Inference
Providers router](https://router.huggingface.co/v1) — the `model` field
is a Hub repo id, optionally with a policy/provider suffix
(`openai/gpt-oss-120b:cheapest`, `deepseek-ai/DeepSeek-R1:sambanova`).
Models that aren't on the serverless router (e.g. JetBrains **Mellum-2**)
won't resolve there — self-host them with vLLM and point a `huggingface`
or `lmstudio` entry at your own `baseUrl`. See
[`examples/mellum.config.yaml`](./examples/mellum.config.yaml).

## Roadmap

What ships in v0.1:
- Single static binary, multi-LLM agent runtime
- Encrypted vault + subprocess sandbox + egress proxy
- Hash-chained audit log + policy engine + canary tokens
- Scheduler with persistent cron jobs
- Personal / team / enterprise mode tiers

What's in flight:
- **Fathom Recall (memory) integration** — Fathom inherits memory from your existing Claude Code / Cursor / Codex sessions via [`zdaniels/recall`](https://github.com/zdaniels/recall) as a sidecar. Tiered memory (episodic / semantic / procedural), 98.9% R@5 on LongMemEval.
- **Sigil (identity) integration** — per-session WIMSE/SPIFFE URIs, RFC 8693 token-exchange delegation chains, real-time revocation via [`fantazmai/sigil`](https://github.com/fantazmai/sigil).
- **Container-per-session sandbox** — Docker / Apple Container backends for skill isolation beyond the current process boundary.

## License

Personal mode (core + security mesh + runtime + skills + scheduler): **MIT**
Team / enterprise modes (RBAC, multitenancy, SSO, compliance exports): **Source-available**, commercial license required for production use.

## Reference implementation

The original TypeScript implementation is preserved at [`fantazmai/fantazm-ts-reference`](https://github.com/fantazmai/fantazm-ts-reference) as a frozen spec.
