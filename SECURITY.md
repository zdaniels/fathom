# Security policy

## Reporting a vulnerability

Use [GitHub private vulnerability reporting](https://github.com/zdaniels/fathom/security/advisories/new).
Include the affected revision, impact, and steps to reproduce. Do not put
credentials, private conversations, or vulnerability details in public issues.

This is a community-maintained project; there is no guaranteed response SLA.
Reports are triaged privately and fixes are published when ready.

## Supported code

Security work targets the current `main` branch. There are no published binary
releases at present.

## Deployment boundary

Each agent instance is a trusted shared workspace. Instance operators may use
policy-allowed host tools; unrelated tenants require separate container/host,
credential, and storage boundaries. Tenant records alone do not provide isolation.
See [deployment boundaries](README.md#deployment-and-isolation), including the
fail-closed Docker mode for offline untrusted skills and OIDC/admin configuration.

Third-party dependencies retain their own security reporting channels.
