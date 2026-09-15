# Privacy

## TL;DR

**Fathom does not phone home.** No analytics, no telemetry, no
crash-reporting service. Your conversations, your files, and your API
keys stay on your machine.

The binary makes outbound network requests in exactly four cases:

1. **LLM calls** — to whatever provider you configured (OpenAI,
   Anthropic, your Ollama). These are direct from your machine to the
   provider's API; we don't proxy them.
2. **Web search tool calls** — to DuckDuckGo's `html.duckduckgo.com`
   when the agent uses `web_search`. No identifier is sent beyond what
   the HTTP client passes by default.
3. **Skill HTTP calls** — for installed skills, through the local
   egress proxy to manifest-declared hosts. The proxy injects the
   credentials; the skill never sees the raw token.
4. **Hub queries** — when you run `fathom hub search`, the CLI talks
   to whatever hub URL you've configured (default
   `http://127.0.0.1:8791` — typically also local).

That's it. Nothing else dials out.

## What's stored where

| Data | Location | Encrypted |
|---|---|---|
| API keys, OAuth tokens | `~/.fantazm/vault` | AES-256-GCM |
| Encrypted vault key | `~/.fantazm/.master.key` | filesystem permissions (0600) |
| Audit log | in-memory only, ring buffer of 10k entries | n/a |
| Conversation history | RAM during the chat session — discarded on exit unless you've installed the Fathom Recall memory sidecar | n/a |
| Schedule definitions | `./data/schedule.db` (project) or `~/.fantazm/schedule.db` (global) | no — local SQLite |
| Memory database | `~/.local/share/recall/recall.db` (if Fathom Recall is installed) | no — local SQLite |
| Notes | in-memory per session (no persistence yet — known limitation) | n/a |

Anything in your prompt or context window is sent to the configured LLM
provider per their privacy policy. We can't help with that — read the
provider's terms before pasting sensitive data in.

## What goes to the LLM provider

When you ask Fathom something, the request body sent to the LLM
provider includes:

- The system prompt (Fathom's baseline security rules + your persona).
- The conversation history for the current session.
- The tool catalog (names + descriptions of registered tools).
- Tool results from prior turns in the session.

It does NOT include:

- Your API keys (those are only used as Authorization headers).
- Files from outside the workspace (the agent can only read what's
  inside `FANTAZM_WORKSPACE_ROOT` or your cwd).
- Your `~/.ssh/`, browser data, or any other system secrets the agent
  can't reach via its tools.

## Rights

You own everything in `~/.fantazm/`. Delete it any time. Re-run
`fathom init` to start over. There's no Fathom-side account or
cookie to revoke.

Questions: privacy@github.com/zdaniels/fathom.
