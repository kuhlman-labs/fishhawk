---
title: CLI
description: The fishhawk command surface — stub, filled by E12.4 (#2264).
---

:::note[This page is a stub]
The per-command reference is written by [E12.4
(#2264)](https://github.com/kuhlman-labs/fishhawk/issues/2264), generated from
the command definitions so it cannot drift from the binary.
:::

`fishhawk` is the command-line interface to a Fishhawk backend. The command
groups today:

| Command | What |
|---|---|
| `init` | Scaffold `.fishhawk/workflows.yaml` from an autonomy preset. |
| `validate` | Validate a workflow document against its declared major. |
| `migrate-spec` | Translate a v1 document to v2 with an eligibility report. |
| `run` | Open and drive runs. |
| `plan` | Read a run's plan. |
| `audit` | Query the audit log. |
| `export` | Export an audit chain for offline verification. |
| `token` | Mint and store a backend credential. |
| `doctor` | Diagnose a local setup. |

`fishhawk <command> --help` is authoritative for flags. The component README is
[`cli/README.md`](https://github.com/kuhlman-labs/fishhawk/blob/main/cli/README.md).
