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

The personal agent runs locally. Team and enterprise modes add persistent roles,
admin APIs, audit storage, and optional OIDC login. The agent team board adds member-scoped workspaces and offline Docker execution
with separate volumes. Personal chat and CLI takeover still run in the instance’s
trusted host workspace. Legacy tenant catalog records do not provision environments.

| Feature | Current implementation |
| --- | --- |
| Terminal chat | Interactive chat, model selection, thread joining, and configuration wizard. |
| Browser UI | Chat at `/`, settings at `/settings`, shared agent board at `/board`, and user administration at `/admin` in team/enterprise mode. |
| Collaboration | Built-in tasks, member roles, builder/reviewer handoffs, human approval, activity history, and optional Linear/Jira issue import and handoff comments. [Setup](docs/collaboration.md). |
| Live responses | Actual text deltas from Claude Code, OpenAI-compatible APIs, Anthropic, and Ollama. Other backends return one completed response. SSE reconnects reconcile persisted messages. |
| Tools | File reads/writes/edits, grep/glob, shell, web search, notes, Docker-based Python, image generation, and macOS system actions. Availability depends on policy and dependencies. |
| Skills | Bundled integrations installed with `fathom install`; Node and Python subprocess runners. |
| Model routing | Named models and optional intent-based profiles with selected tools. Unavailable credentials are skipped per named model. |
| External coding agents | Optional Claude Code or Codex tool/takeover execution using locally installed, authenticated CLIs. |
| Memory | Optional [Recall](https://github.com/zdaniels/recall) sidecar; not bundled or required for basic chat. |
| Scheduling | SQLite-backed jobs and routines; cron or `--every` / `--at`; gateway must remain running. |
| Threads and devices | SQLite-backed threads and hashed device tokens survive restarts. |
| Vault | Encrypted secret storage with a local keyfile or supported OS keychain. |
| Audit | Hash-chained SQLite events survive restarts; `fathom audit` reads the same data directory. |
| Team/enterprise | Role and tenant administration plus audit-report exports; important limitations below. |

There are currently no published releases or Homebrew packages for this
repository. Build from source. Release configuration and installers are present
for future releases; they do not themselves provide downloadable binaries.

## Build and run

Requires Go 1.26 or newer. Node 22.6+ is needed for TypeScript skills; Python
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
that need a restart. User accounts and global roles are managed at `/admin`; workspace membership is managed at `/board`.

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

Readiness is based on the actual provider registry. An all-local registry needs
no unrelated hosted-provider key. Missing credentials skip optional named models;
unknown providers are configuration errors. If every model lacks credentials,
the agent starts in echo mode and `/api/v1/ready` returns 503.

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
built-in tools, vault, and security mesh, including its persistent audit logger.

Threads are user-scoped and persisted. Deletion is soft; an automatic permanent
purge is not implemented. Device pairing shares the initiating user's identity;
it is not an invitation that creates an independent team account.

## Agent team board

Open `/board` to create a workspace and tasks. A builder works on the task,
a reviewer inspects its files read-only, and a person approves completion.
The built-in board needs no external task service. Optional workspace-scoped
Linear and Jira Cloud connections import issues and publish handoff comments
only when a member requests it. [Configuration and boundaries](docs/collaboration.md).

Claude Code and Codex takeover conversation IDs persist privately in
`dataDir/takeover-sessions`, scoped by provider, model, workspace, user, and
conversation. REST and scheduled calls remain independent. Provider session
files must also remain available to the installed CLI.

## Team and enterprise: free, but experimental

There is one codebase and no paid edition. Enable the existing enterprise APIs
by setting `mode: enterprise`, or override your chosen config at startup:

```bash
FATHOM_CONFIG=/absolute/path/fathom.config.yaml FATHOM_MODE=enterprise fathom start
```

Use `team` instead of `enterprise` for team mode. Stop an existing gateway before
starting another on the same port. A restart is required to change modes.
`fathom init --enterprise` writes a starter configuration. Configure your identity
provider separately if you want SSO; token authentication works without SSO.

| Area | What is available |
| --- | --- |
| Admin access | Bootstrap `admin` role; bearer authentication; role checks on admin and settings mutations. |
| Roles | SQLite-backed admin/operator/viewer assignments. Only instance admins and operators can invoke the agent, including streams and thread messages. Viewers can inspect their own history. |
| Workspaces | Board tasks enforce membership and run in separate offline Docker volumes. One run per workspace, four per instance, bounded CPU/memory/turns/time. Legacy tenant catalog settings do not configure these resources. |
| SSO/OIDC | Browser sign-in and admin step-up, discovery, verified signed tokens, issuer/audience/expiry/nonce checks, browser-bound state, PKCE, and expiring gateway sessions. |
| Reports | JSON/CSV event exports labelled SOC2/HIPAA/GDPR; these are event summaries, not certifications or compliance assessments. |
| Persistence | Roles/tenants in `dataDir/enterprise.db`; agent audit chain in `dataDir/audit.db`. Protect and back up this directory. |
| Hardening | Validated IP allow-lists and rate limits cover admin and settings. Optional step-up requires fresh OIDC authentication. |

### Enable OIDC login

Register the exact callback URL with your identity provider, then configure:

```yaml
mode: enterprise
enterprise:
  sso:
    issuer: https://identity.example.com
    clientId: fathom
    clientSecret: YOUR_CLIENT_SECRET
    redirectUri: https://agent.example.com/api/v1/sso/callback
  admin:
    requireStepUp: true
    allowCidrs: ["127.0.0.1/32"]
```

Store this configuration privately (or use a Kubernetes Secret). Configure your
reverse proxy/TLS and allow-list for your actual network. CIDRs match the immediate
peer, not forwarded headers; enforce client CIDRs at the ingress behind a proxy.
HTTP issuer/callback URLs are allowed only for loopback development.

Open `/api/v1/sso/login` in a browser. The callback returns JSON with a one-hour
`token` and stable `user.id`; paste the token into the existing browser token
prompt or use it as a bearer token. There is no automatic role elevation from
email or group claims. A bootstrap admin assigns the returned ID an instance
role through `/api/v1/admin/users/role` (omit `tenantId`). New SSO users are viewers.
SSO sessions expire after one hour and on restart; they cannot mint persistent
paired-device tokens.

Browser login stores the session in the same-origin chat UI and opens the admin
page for administrators. API clients receive JSON. The admin and settings UIs
consume a stored step-up grant automatically; verify again before another
protected mutation.

A fresh admin login also returns a `stepUpToken`, valid for five minutes and one
mutation. Send it as `X-Step-Up-Token` with that same user's bearer token. Enable
`requireStepUp` **after** assigning your first OIDC admin using the bootstrap
admin token. Reusing an ordinary admin token as step-up is rejected. The identity
provider must return a recent `auth_time` when asked for fresh authentication.

Enterprise features are all MIT-licensed. There is no purchase or license key.

## HTTP API

Except health/readiness, OIDC login/callback, and static UI assets, the following application endpoints require
a bearer token; pairing claim uses its short-lived code instead.

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/v1/health` | HTTP liveness and feature information |
| GET | `/api/v1/ready` | 200 when a backend is configured, 503 in echo mode; does not probe upstream availability |
| POST | `/api/v1/message` | Send `{"text":"Hello"}` |
| POST | `/api/v1/stream` | Forward actual response text; `done.reply` is the authoritative final answer |
| GET/POST | `/api/v1/board` | List/create member workspaces |
| GET/POST | `/api/v1/board/{workspace}/…` | Tasks, members, handoffs and integrations; [API](docs/collaboration.md) |
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
and `/api/v1/audit`. Configured enterprise SSO mounts `/api/v1/sso/login` and
`/api/v1/sso/callback`.

`fathom audit --json` reads and verifies `dataDir/audit.db` without starting a
model or sidecar. Use the same `FATHOM_CONFIG` as your running agent. The audit
API accepts `limit` from 0 to 10000. Audit writes are transactional; storage
failures stop model/tool dispatch and admin mutations. Back up the database;
daily maintenance retains 90 days of audit history and permanently purges chats
30 days after deletion. Set `retention.auditDays` and
`retention.deletedThreadDays` (0 disables; maximum 36500). A verified hash
checkpoint anchors the retained audit suffix. Retention also runs at startup.
Hash chains detect edits within the
stored history, but cannot detect wholesale replacement by a privileged host
administrator without an externally retained checkpoint.

## Deployment and isolation

The default listener is loopback. LAN access requires an appropriate bind
address. Terminate HTTPS with your ingress or reverse proxy.

### Skills

The default subprocess mode is for **trusted installed code**. Its optional OS
wrappers protect selected files but are not a complete boundary for hostile code.
For offline untrusted skills, set `FATHOM_SKILL_SANDBOX=required`. This requires a
trusted local Docker daemon and uses disposable non-root containers with no
network, a read-only filesystem, resource limits, and read-only mounts of only
the runner and selected skill directory. Missing Docker or a failed container
stops the invocation; it never falls back to a host subprocess. This mode does
not support network-dependent skills or persistent writes. Do not place secrets
inside skill directories. Only explicitly supplied invocation secrets are exposed.

`FATHOM_SKILL_SANDBOX=trusted` explicitly selects the trusted subprocess mode;
`1` retains the legacy Linux bubblewrap hardening option. Unknown values fail.
The external Chasm runtime is separately configured and must enforce its own
isolation. Charon, Chasm, Beacon, and Sigil services are not bundled.

### Docker / Compose

Edit `deploy/container.config.yaml` and `deploy/container.policy.yaml`, then run:

```bash
docker compose up --build -d
```

The example uses a host Ollama server and `llama3.2` (install the model first).
For hosted models, select the provider and supply credentials via environment or
an untracked env file. The browser UI is `http://127.0.0.1:8790`. State, vault,
threads, devices, and cache live on the `/data` volume. The config/policy mounts
are read-only: edit the source files and restart to change them. Do not mount
your host home or Docker socket into the agent container.

### Helm

Build and push an image to a registry you control, then supply its repository
and tag explicitly. The chart does not assume an unpublished `latest` image.

```bash
helm upgrade --install fathom ./helm \
  --set image.repository=YOUR_REGISTRY/fathom --set image.tag=YOUR_TAG \
  --set credentialsSecret=fathom-provider-keys
```

Create `fathom-provider-keys` first with the required provider environment keys.
Set `config.llm` in values for model configuration. For private configuration
such as OIDC credentials, set `configSecret` to a Secret containing `config.yaml`.
The chart creates a PVC, mounts config/policy, and provisions writable `/data`
and `/tmp` for UID/GID 65532. It requires one replica per instance; use separate
releases for unrelated tenants. Unsupported KMS/SIEM/auth-mode values were removed.
CI builds and smoke-tests the container and validates chart rendering. This
repository does not yet publish a container image.

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
node --test internal/gateway/web/chat.test.cjs
go build ./...
go vet ./...
go test -race -count=1 -timeout=5m ./...
```

CI checks Linux/macOS builds and race tests, release configuration, container
startup, and Helm rendering. OIDC tests use a local signed-token test provider;
provider integrations still need credentials and live-service validation for your
deployment. A passing test suite is not a security certification.

See [CONTRIBUTING.md](CONTRIBUTING.md), [ARCHITECTURE.md](ARCHITECTURE.md), and
[SECURITY.md](SECURITY.md). Some architecture comments describe intended future
behavior; this README states the current supported behavior and known limits.

## License

[MIT](LICENSE.md), covering all Fathom project code and all deployment modes.
Use, modification, redistribution, hosting, and commercial use are permitted
subject to the license's notice requirements. Third-party dependencies retain
their respective licenses. There is no separate enterprise license.
