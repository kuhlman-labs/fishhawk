---
title: Reference
description: The workflow spec, the plan artifact, the CLI, the REST API — and which spec majors are live.
---

Reference material for the surfaces you write against. Each page here links out
to the canonical source under
[`docs/`](https://github.com/kuhlman-labs/fishhawk/tree/main/docs) in the
repository rather than inlining it, which is what keeps the two from drifting.

| Page | What |
|---|---|
| [Workflow spec](/fishhawk/reference/workflow-spec/) | `.fishhawk/workflows.yaml` — the document that declares your workflows. |
| [Plan schema](/fishhawk/reference/plan-schema/) | The `standard_v1` plan artifact. |
| [CLI](/fishhawk/reference/cli/) | The `fishhawk` command surface. |
| [API](/fishhawk/reference/api/) | The v0 REST API. |
| [Versioning](/fishhawk/reference/versioning/) | How this site is versioned, and how spec majors are carried. |

## Workflow spec major support

Three majors of the workflow spec are live at once. They are not three release
lines — a single `main` accepts all three, and which one your document routes to
is decided by its `version:` field. This table is the status of each; the
differences themselves are called out inline on the [workflow spec
page](/fishhawk/reference/workflow-spec/).

| Major | Declare it as | Status |
|---|---|---|
| `workflow-v0` | `version: "0.3"` … `"0.7"` | **Frozen.** Still embedded, routed, and validated — and it will stay that way. A `0.x` document keeps working with no action. Not the right choice for a new spec. |
| `workflow-v1` | `version: "1.0"` … `"1.6"` | **Frozen.** Same posture as v0: accepted forever, still validated, no removal planned. `fishhawk migrate-spec` translates a v1 document to v2 and prints what changed. |
| `workflow-v2` | `version: "2"` | **Live.** What `fishhawk init` emits and what a new spec should declare. The reuse, legibility, and autonomy grammar exists only here. No minor chain — a field is accepted because the v2 schema declares it. |

**No removal date is set for v0 or v1, and none is planned.** Carrying every
major forever is what keeps old runs in the audit log readable, which is the
whole point of recording them. A new spec on v0 or v1 is accepted; it is
discouraged because you would be opting out of the current grammar, not because
it will stop working.

The canonical, agent-consumed field reference for each major lives in
[`docs/spec/`](https://github.com/kuhlman-labs/fishhawk/tree/main/docs/spec):
[`workflow-v2.md`](https://github.com/kuhlman-labs/fishhawk/blob/main/docs/spec/workflow-v2.md)
is the complete standalone reference for the live major, and
[`workflow-v0.md`](https://github.com/kuhlman-labs/fishhawk/blob/main/docs/spec/workflow-v0.md)
and
[`workflow-v1.md`](https://github.com/kuhlman-labs/fishhawk/blob/main/docs/spec/workflow-v1.md)
are the two frozen ones — read them to understand an existing document, not to
look up a v2 field.
