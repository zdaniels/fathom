<p align="center">
  <img src="assets/logo/fathom-logo-400.png" alt="Fathom" width="240" />
</p>

# Fathom

A self-hosted AI agent with a terminal interface, browser chat, local tools,
scheduled tasks, and optional shared memory. The runtime is written in Go.

**All Fathom modes—personal, team, and enterprise—and the standalone gateway
are free software under the [MIT license](LICENSE.md).** No license key,
subscription, or paid feature unlock is required. Model APIs, external agent
subscriptions, and infrastructure can have their own costs.

[Source](https://github.com/zdaniels/fathom) ·
[CI](https://github.com/zdaniels/fathom/actions/workflows/ci.yml) ·
[Security reporting](SECURITY.md)

## Current status

The personal agent runs locally. Team and enterprise modes expose additional
admin APIs, but their isolation, persistence, and SSO are incomplete. They are
experimental, not ready for a shared production service or untrusted tenants.

| Feature | Current implementation |
| --- | --- |
| Terminal chat | Interactive chat, model selection, thread joining, and configuration wizard. |
| Browser UI | Chat at `/`; settings at `/settings`; pairing and installable PWA assets. |
| Tools | File reads/writes/edits, grep/glob, shell, web search, notes, Docker-based Python, image generation, and macOS system actions. Availability depends on policy and dependencies. |
| Skills | Bundled integrations installed with `fathom install`; Node and Python subprocess runners. |
| Model routing | Named models and optional intent-based profiles with selected tools. See the provider startup limitation below. |
| External coding agents | Optional Claude Code or Codex tool/takeover execution using locally installed, authenticated CLIs. |
| Memory | Optional [Recall](https://github.com/zdaniels/recall) sidecar; not bundled or required for basic chat. |
| Scheduling | SQLite-backed jobs and routines; cron or `--every` / `--at`; gateway must remain running. |
| Threads and devices | SQLite-backed threads and hashed device tokens survive restarts. |
| Vault | Encrypted secret storage with a local keyfile or supported OS keychain. |
| Audit | Hash-chained, bounded **in-memory** events; no durable agent audit store. |
| Team/enterprise | Role and tenant administration plus audit-report exports; important limitations below. |

There are currently no published releases or Homebrew packages for this
repository. Build from source. Release configuration and installers are present
for future releases; they do not themselves provide downloadable binaries.

## Build and run

Requires Go 1.25 or newer. Node 22.6+ is needed for TypeScript skills; Python
skills require Python 3. Docker is optional and needed for `python_exec`.

```bash
git clone https://github.com/zdaniels/fathom.git
cd fathom
go build -o fathom ./cmd/fathom
./fathom init
./fathom chat
```

The wizard writes configuration and policy files and asks for your model setup.
An Ollama model must already be installed and its server running. Hosted models
need the corresponding credentials. `./fathom doctor` diagnoses configuration,
vault, providers, and optional dependencies; it may flag unused model entries.

For browser chat, run the gateway instead:

```bash
./fathom start
```

Default local URLs:

- Chat: <http://127.0.0.1:8790/>
- Settings: <http://127.0.0.1:8790/settings>
- Health: <http://127.0.0.1:8790/api/v1/health>

The browser requires a paired device token or the local API token. On first boot,
the gateway creates an admin token and writes it to `~/.fantazm/api-token`.
Keep that token private. A successful health response means the HTTP server is
up; it does **not** prove a model is configured or can answer.

Optionally install the executable on your PATH:

```bash
mkdir -p "$HOME/.local/bin"
install -m 755 fathom "$HOME/.local/bin/fathom"
# Ensure ~/.local/bin is on your PATH.
```

The macOS-only `fathom service install` command registers a launchd service.
Its working directory affects configuration discovery. If you use a custom
config/environment, verify the generated service definition preserves it before
relying on automatic startup. Running `fathom start` in a terminal works without
installing a service.

## Configuration

Fathom searches upward for `fathom.config.yaml`, then checks global locations.
Legacy configuration names and environment variables are supported; see
[configuration compatibility](COMPATIBILITY.md).

Use `FATHOM_CONFIG` to pin a config consistently for both startup and the
settings API:

```bash
FATHOM_CONFIG=/absolute/path/fathom.config.yaml fathom start
```

A local single-model example:

```yaml
mode: personal
host: 127.0.0.1
port: 8790
llm:
  provider: ollama
  model: YOUR_INSTALLED_MODEL  # replace with a name from `ollama list`
dataDir: ./data
policyFile: ./fathom.policy.yaml
```

A minimal policy:

```yaml
version: 1
defaults:
  network: deny
  filesystem: read-only
  shell: deny
  secrets: isolated
rules: []
```

Policy defaults restrict tools. Enable the capabilities you actually need;
installing a skill does not automatically grant every permission it requires.
Personal-mode settings edits are restricted to local callers with authentication.
The settings page supports policy changes immediately and marks config changes
that need a restart. It is not a complete enterprise user/tenant administration UI.

Common runtime overrides include `FATHOM_MODE`, `FATHOM_HOST`, `FATHOM_PORT`,
`FATHOM_DATA_DIR`, `FATHOM_VAULT_PATH`, `FATHOM_VAULT_KEY`,
`FATHOM_SCHEDULE_DB`, and `FATHOM_SKILLS_DIR`. Several stores have their own
path overrides; `dataDir` does not relocate all state. Existing `.fantazm` storage
and service identifiers remain supported to preserve installed configurations.

### Providers

The source contains adapters for the following provider IDs. Configure a model
that your provider actually offers; adapter support does not guarantee access
to every model. Credentials resolve from the vault or environment.

| Provider ID | Credentials |
| --- | --- |
| `anthropic` | `ANTHROPIC_API_KEY` |
| `openai` | `OPENAI_API_KEY` |
| `ollama` | None; local Ollama server |
| `lmstudio` | Local OpenAI-compatible server |
| `gemini` | `GEMINI_API_KEY` |
| `bedrock` | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`; optional `AWS_SESSION_TOKEN`; region via configuration/environment |
| `deepseek` | `DEEPSEEK_API_KEY` |
| `xai` | `XAI_API_KEY` |
| `xiaomi`, `mimo` | `XIAOMI_MIMO_API_KEY` |
| `groq` | `GROQ_API_KEY` |
| `openrouter` | `OPENROUTER_API_KEY` |
| `together` | `TOGETHER_API_KEY` |
| `fireworks` | `FIREWORKS_API_KEY` |
| `mistral` | `MISTRAL_API_KEY` |
| `huggingface`, `hf` | `HF_TOKEN` |

Named models use `llm.models` and `llm.default`. Optional `routing.profiles`
select models and tool sets based on an intent classifier.

**Known startup issue:** the non-routed factory checks a legacy provider key
before constructing the named-model registry. A local-only named-model config
can incorrectly require an Anthropic key; some other providers can incorrectly
require an OpenAI key. For an all-Ollama configuration, explicitly set
`llm.provider: ollama` as well as the named entries. The provider gate needs a
code fix; do not add an unrelated paid-provider key just to satisfy it.

### External-agent takeover

```yaml
takeover:
  enabled: true
  provider: claude  # or codex
```

Restart after changing takeover settings. The selected CLI must already be
installed and signed in. This sends interactive chat to that CLI instead of
Fathom's own agent loop. The external CLI's permissions and execution behavior
apply; this path does not inherit all of Fathom's tool-policy protections.
Claude supports session continuation; Codex's current adapter does not preserve
conversation sessions between calls.

Use takeover only for a trusted single user at present. Cross-user REST session
isolation needs fixing. Scheduled tasks currently use the ordinary local agent
handler even when interactive takeover is enabled.

## Tools, skills, routines, and threads

```bash
fathom install                       # list bundled skills
fathom install browser               # install a named integration
fathom skills list
fathom routine add briefing "Summarize the project status"
fathom routine run briefing
fathom schedule add briefing --every weekday --at 9am
fathom schedule list
fathom join --list
fathom join
fathom export --help
```

Use `fathom <command> --help` for exact flags. Integrations need their external
accounts, credentials, permissions, and dependencies. Some OAuth setup flows
are incomplete and require manual vault configuration.

Scheduled results are written to the job store and delivered to console/events.
They are not automatically sent by email or Slack. `--skill` adds a skill
instruction to the scheduled prompt; it is not a separate isolation boundary.

The default profile includes the scheduler, threads, skill bridging, and optional
memory. The minimal profile skips those integrations but retains the agent,
built-in tools, vault, and security mesh, including its in-memory audit logger.

Threads are user-scoped and persisted. Deletion is soft; an automatic permanent
purge is not implemented. Device pairing shares the initiating user's identity;
it is not an invitation that creates an independent team account.

## Team and enterprise: free, but experimental

There is one codebase and no paid edition. Enable the existing enterprise APIs
by setting `mode: enterprise`, or override your chosen config at startup:

```bash
FATHOM_CONFIG=/absolute/path/fathom.config.yaml FATHOM_MODE=enterprise fathom start
```

Use `team` instead of `enterprise` for team mode. Stop an existing gateway before
starting another on the same port. A restart is required to change modes.
`fathom init --enterprise` also writes a starter configuration, but cannot make
the unfinished SSO integration functional.

| Area | What is actually available |
| --- | --- |
| Admin access | Bearer-token authentication; bootstrap `admin` role; role-gated admin endpoints and settings writes. |
| Roles | Admin/operator/viewer assignments in memory. Tool-level role enforcement is incomplete. |
| Tenants | In-memory records and configuration. Not connected to isolated agent workspaces or enforced tenant quotas. |
| SSO/OIDC | Helper code exists, but login/callback routes and gateway authentication integration are missing. Configuring SSO does not enable a working login. |
| Reports | JSON/CSV audit-event exports labelled SOC2/HIPAA/GDPR. These are not certifications or framework-specific compliance assessments. |
| Persistence | Role/tenant assignments and agent audit events are lost on restart. |
| Hardening | Admin network allow-lists, rate limits, and token re-authentication exist, but settings coverage and validation need fixes. |

Keep team/enterprise deployments local and restricted to trusted users until
these gaps are addressed. No purchase will unlock missing functionality.

## HTTP API

Except health and static UI assets, the following application endpoints require
a bearer token; pairing claim uses its short-lived code instead.

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/v1/health` | HTTP liveness and feature information |
| POST | `/api/v1/message` | Send `{"text":"Hello"}` |
| POST | `/api/v1/stream` | Stream an agent reply |
| GET | `/api/v1/session` | Caller sessions |
| GET / POST | `/api/v1/threads` | List/create threads |
| GET / PATCH | `/api/v1/settings` | Inspect/update supported settings |
| POST | `/api/v1/pair/start` | Create a pairing code |
| GET | `/api/v1/pair/watch` | Wait for pairing |
| POST | `/api/v1/pair/claim` | Claim a pairing code |
| GET | `/api/v1/devices` | List caller devices |
| DELETE | `/api/v1/devices/{id}` | Revoke a device |

Team and enterprise additionally mount `/api/v1/admin/users`,
`/api/v1/admin/users/role`, `/api/v1/admin/tenants`, `/api/v1/admin/compliance`,
and `/api/v1/audit`. There is no working `/api/v1/sso/*` login flow.

The agent audit API reads only the running process's retained entries.
`fathom audit` currently constructs a separate logger, so it cannot retrieve
past gateway/chat activity. Persistent audit storage and a connected CLI are
still needed.

## Deployment and isolation limits

- The default listener is loopback. LAN access requires an appropriate bind
  address. There is no built-in TLS termination.
- Pairing, Tailscale address detection, an optional Cloudflare Tunnel helper,
  and an external relay client exist. External services need their own setup.
- Skill subprocess isolation depends on the OS and installed tools. Linux
  bubblewrap support is opt-in; unsupported or missing sandbox tools fall back
  to an ordinary subprocess. Treat installed skills as trusted code.
- Docker and Helm files are starting points, not a verified production install.
  The Compose policy mount is not selected by its current defaults. Helm lacks
  complete writable state/cache mounts and provisioning for credentials;
  `auth.mode`, `encryption.kms`, and `audit.siem` values are not wired through.
  No container image is currently published by this repository's release workflow.
- Charon, Chasm, Beacon, and Sigil clients exist, but those external services are
  not bundled in this repository. Fathom's core can run without them.

## Separate LLM proxy binary

```bash
go build -o fathom-gateway ./cmd/fathom-gateway
./fathom-gateway --config /absolute/path/config.yaml
```

This is a separate OpenAI-compatible LLM proxy, not the browser agent gateway.
It has its own [configuration schema](examples/fathom-gateway.config.yaml),
provider endpoints, tenant bearer tokens, model allow-lists, and quota tracking.
Anthropic-native/Bedrock-native request translation and persistent quota storage
are not implemented. It is included under the same MIT license.

## Development

```bash
go build ./...
go vet ./...
go test -race -count=1 -timeout=5m ./...
```

CI checks Linux and macOS plus the release configuration. A local Docker test
currently misclassifies “Docker installed but daemon unavailable” as “Docker
missing”; this can fail even when CI passes. Tests do not constitute a security
audit or prove every provider/integration works against its live service.

See [CONTRIBUTING.md](CONTRIBUTING.md), [ARCHITECTURE.md](ARCHITECTURE.md), and
[SECURITY.md](SECURITY.md). Some architecture comments describe intended future
behavior; this README states the current supported behavior and known limits.

## License

[MIT](LICENSE.md), covering all Fathom project code and all deployment modes.
Use, modification, redistribution, hosting, and commercial use are permitted
subject to the license's notice requirements. Third-party dependencies retain
their respective licenses. There is no separate enterprise license.
