---
title: API
description: The v0 REST API — stub, filled by E12.4 (#2264).
---

:::note[This page is a stub]
The rendered API reference is written by [E12.4
(#2264)](https://github.com/kuhlman-labs/fishhawk/issues/2264), generated from
the OpenAPI document so the published surface and the served one cannot
diverge.
:::

`fishhawkd` serves a versioned REST API under `/v0`. Runs, stages, plans,
approvals, scope amendments, and the audit log are all reachable through it —
the CLI, the Web UI, and the MCP server are three clients of this one surface.

The source of truth is
[`docs/api/v0.openapi.yaml`](https://github.com/kuhlman-labs/fishhawk/blob/main/docs/api/v0.openapi.yaml);
the human companion is
[`docs/api/v0.md`](https://github.com/kuhlman-labs/fishhawk/blob/main/docs/api/v0.md).

`GET /healthz` needs no credential and reports the build's commit and its
embedded schema hashes. Everything else requires a bearer token.
