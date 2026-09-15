# Agent team board

All collaboration and enterprise code is MIT licensed. No license key, subscription,
paid build, or feature flag is required. Model providers and external services can
have their own charges.

## Start with built-in tasks

1. Run `fathom start` and open `/board` on the same gateway as your chat UI.
2. Create a workspace, then a task with an outcome and acceptance criteria.
3. Assign a human owner and optional named builder/reviewer models.
4. Mark it ready and click **Start builder**. Starting work is always explicit.
5. Inspect the builder's handoff: changes, verification, decisions, open questions.
6. Click **Run reviewer**. The reviewer sees the same files through a read-only mount.
7. Request changes or approve completion. An agent cannot approve for a person.

Tasks move through backlog, ready, running, review, blocked, and done. A new
builder run invalidates the previous review. Editing the brief invalidates both
handoff and review. Concurrent updates use revisions: a stale edit returns HTTP
409 instead of overwriting a teammate's changes. Only one run can write a workspace
at a time. The board refreshes every four seconds and records persistent activity.
Private chat history remains private; adding workspace members does not share it.

## Add people

Set `mode: team` (or `enterprise` for OIDC) in the instance config and restart.
At `/admin`, sign in with the bootstrap admin token and create an account.
The new account starts as a viewer. Give its token to the teammate privately,
then add the returned user ID as a workspace member at `/board`. Keep the global
viewer role for teammates who should only use isolated workspace agents. Global
operator access also permits the trusted host agent and is not needed for the board.

Workspace roles are independent of global roles:

| Workspace role | Access |
| --- | --- |
| Viewer | Read tasks, members, activity and workspace archives |
| Member | Create/edit tasks, run isolated builders/reviewers, comment, request changes, approve, import/publish linked tasks |
| Admin | Member access plus adding/removing members and changing workspace roles |

Creating a workspace requires global operator/admin permission in team/enterprise
mode. Running its isolated builders/reviewers requires workspace membership only;
it does not grant access to host-agent tools. Membership is checked again
between model/tool calls. Revocation does not roll back an already executed
command. Every workspace must retain an administrator. Global administrators do
not implicitly become workspace members.

OIDC users can use the organization sign-in link. Their first sign-in creates a
viewer assignment; a workspace administrator can grant workspace membership.
Fresh admin OIDC login provides one short-lived step-up grant for protected admin
or settings writes. With `requireStepUp` enabled, verify again for the next write.

## Enable isolated agent runs

The board itself works without Docker or model credentials. Runs require Docker
and a native provider in the existing `llm` config (including local Ollama).
Claude/Codex CLI subscription logins are used by personal takeover chat, not by
board workers.

```sh
docker build -t fathom:local .
```

```yaml
collaboration:
  image: fathom:local
```

Restart the gateway after changing configuration. The named image must already
exist on the Docker host; runs never pull images automatically. Use the project's
Dockerfile, which creates `/workspace` owned by UID/GID 65532. That ownership is
copied into each new workspace volume.

The coordinating Fathom process calls the chosen LLM with its vault/environment
credentials. The model's only tool is a shell in the task container. No host-agent
tool registry, personal chat tools, Recall integration, or host filesystem is
available to board workers. Each workspace starts empty; builders can create files
from the task brief and continue from prior tasks' files. Download the result with
**Download workspace archive** (32 MB compressed limit). Repository cloning,
dependency downloads, and archive uploads are not provided by this offline runner.

### Execution boundary

- One distinct Docker volume per workspace; tasks in the same workspace share files.
- No host bind mounts, Docker socket, provider secrets, or container networking.
- Non-root UID/GID 65532, read-only root, dropped capabilities, no new privileges.
- One CPU, 512 MB RAM, 128 processes, and 128 MB temporary storage per run.
- Up to four active runs per instance and one per workspace.
- Twenty model turns; up to eight tool calls per turn; 60 seconds per command;
  20 minutes for the run. A command timeout stops the run and removes its container.
- Reviewer workspace mounts are read-only. Temporary files can be written under `/tmp`.
- Gateway interruption marks unfinished tasks blocked; it never auto-replays work.
  Retry requires a person to inspect the workspace and start another run.

Workspace volume disk space and provider spend are not hard-quota controlled.
Operate one gateway process per data directory. Use separate deployments and
Docker hosts for mutually hostile customers, host administrators, or requirements
for independent credentials, billing and hard disk quotas. Docker is a container
boundary, not a separate kernel. The legacy `/admin/tenants` catalog does not
provision board workspaces or change personal chat/CLI execution.

Board state is stored in `dataDir/collaboration.db`; files are in Docker volumes
named `fathom-workspace-<hash-of-workspace-id>`. Back up both. Board tasks/activity
and workspace volumes have no automatic deletion policy. Do not delete a volume
while a workspace run is active.

## Connect Linear or Jira Cloud

The instance owner configures each connection for one workspace and one external
team/project. Copy the workspace ID from its members section. Store credentials
in the Fathom vault or process environment; config contains only the secret name.
Connections and secrets are never exposed to task containers. Workspace members
can import any issue in that configured scope, so use a service account limited
to the intended team/project.

```yaml
collaboration:
  image: fathom:local
  connections:
    - workspaceId: WORKSPACE_ID
      provider: linear
      scope: LINEAR_TEAM_UUID
      tokenSecret: LINEAR_TEAM_API_KEY
    - workspaceId: WORKSPACE_ID
      provider: jira
      site: https://your-team.atlassian.net
      email: service-account@example.com
      scope: PROJECTKEY
      tokenSecret: JIRA_TEAM_API_TOKEN
```

After restart, choose the connection and enter an issue key such as `TEAM-123`.
Import copies the title/brief and retains a link to the source. Reimporting the
same issue returns the existing task. Linear API errors are checked even when
HTTP status is 200. Jira rich text is converted to plain text for the task brief.

**Preview external comment** shows exactly the handoff/review that will be sent.
**Publish this comment** adds that comment to the linked issue after rechecking
its team/project. Nothing is published merely by running or approving an agent.
The integration does not change external status, overwrite descriptions, poll
external changes, install webhooks, or provide OAuth app installation. If a write
times out, inspect the linked issue before retrying to avoid duplicate comments.
Configure at most one connection per provider per workspace.

Protocol references: [Linear GraphQL](https://linear.app/developers/graphql),
[Jira issues](https://developer.atlassian.com/cloud/jira/platform/rest/v3/api-group-issues/),
[Jira comments](https://developer.atlassian.com/cloud/jira/platform/rest/v3/api-group-issue-comments/).

## API

All board endpoints use the same Fathom bearer token as chat.

| Method | Path after `/api/v1/board` | Body / result |
| --- | --- | --- |
| GET | empty | Member workspaces, available model names, run setup message, user ID |
| POST | empty | `{ "name": "Team" }` → workspace |
| GET | `/{workspace}` | Tasks, members, recent activity, non-secret connections |
| POST | `/{workspace}/members` | `{ "userId": "…", "role": "member" }`; empty role removes |
| POST | `/{workspace}/tasks` | Title, description, assignee, builder, reviewer |
| POST | `/{workspace}/import` | `{ "provider": "linear", "id": "TEAM-123" }` |
| POST | `/{workspace}/tasks/{task}/{action}` | Current `revision`, optional `text`; edit also takes task fields |
| GET | `/{workspace}/archive` | Workspace `.tar.gz` |

Actions: `edit`, `ready`, `comment`, `changes`, `build`, `review`, `cancel`,
`approve`, `publish`. Builder/reviewer starts return 202. Only cancellation is
accepted during an active run. Read responses contain up to 1,000 tasks and the
200 most recent activity records; older activity remains in the database.

## Tests

```sh
go test -race ./internal/collab
FATHOM_TEST_WORKSPACE_IMAGE=fathom:local go test -race -run TestWorkspaceContainers ./internal/collab
```

The integration test uses a fake model and real containers to verify filesystem,
network, secret, non-root, cross-workspace and reviewer boundaries. CI runs it
against the freshly built image. Connector tests use fake transports and do not
contact your Linear or Jira account.
