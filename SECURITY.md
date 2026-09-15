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

The current agent is intended for trusted local use. Team/enterprise isolation,
SSO, audit persistence, and admin hardening remain incomplete. Review the
[current limitations](README.md#team-and-enterprise-free-but-experimental)
before deployment. Do not treat optional subprocess wrappers as a complete
sandbox for hostile third-party code.

Third-party dependencies retain their own security reporting channels.
