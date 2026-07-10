# Release loop (operator verbs)

The operator flow for the delegating `release` workflow (E33 / ADR-051): turn a
range of merged work into a published GitHub Release. The loop is
**prepare → preview → cut → (human-led tag push) → publish**. Each step is a
distinct, gated operator action — Fishhawk records the decisions and renders the
notes, but it deliberately performs **no git tag push and no destructive git
action**; the tag push between the cut and the release pipeline stays a human
git action (see [The tag push is yours](#the-tag-push-is-yours)).

`fishhawk_get_run_status`'s `next_actions` block is authoritative: for a
`release`-workflow run it names the correct verb at each loop state
(`notes_ready` → `awaiting_cut` → `pipeline_running` → `awaiting_publish` →
`published`). Prefer it over memorizing the sequence.

## Surfaces

| Step | MCP | CLI | Endpoint |
| --- | --- | --- | --- |
| Preview notes (read-only) | `fishhawk_release_notes` `mode: "preview"` | `fishhawk release preview` | `GET /v0/releases/notes/preview` |
| Prepare notes (persist artifact) | `fishhawk_release_notes` `mode: "prepare"` | `fishhawk release prepare` | `POST /v0/releases/notes` |
| Cut the version (record decision) | — (CLI only) | `fishhawk release cut` | `POST /v0/releases/cut` |
| Publish to the GitHub Release | — (CLI only) | `fishhawk release publish` | `POST /v0/releases/publish` |

The MCP surface is exactly **one** tool — `fishhawk_release_notes`, the
prepare/preview pair. The **cut** and **publish** steps are CLI verbs (they
record the version decision and write the Release body); `next_actions` names
them at the `awaiting_cut` and `awaiting_publish` states so a driving agent
still knows the next move without an MCP tool for each.

## The loop, state by state

The states below are the `next_actions` `state` values for a `release` run.
They are derived (display-only) from the persisted `release_notes` artifact, the
`release_cut` / `release_published` audit entries, and the deploy stage state.

### 1. `notes_ready` → prepare

No `release_notes` artifact is persisted yet. Render and persist the notes for
the release range:

```sh
fishhawk release prepare --repo owner/name --from v1.1.0 --to HEAD --stage-id <deploy-stage-id>
```

Or over MCP:

```
fishhawk_release_notes { mode: "prepare", repo: "owner/name", from: "v1.1.0", to: "HEAD", stage_id: "<deploy-stage-id>" }
```

`prepare` renders the merged-run evidence in the `from..to` range — including the
advisory **semver bump hint** (E33.4: `suggested bump: <level> (because …)`) — and
persists it as a `release_notes` artifact keyed to `stage_id`. Use
`mode: "preview"` (or `fishhawk release preview`) first for a read-only render
that persists nothing.

### 2. `awaiting_cut` → preview, then cut

The notes are persisted but the version decision is not recorded. Preview the
rendered notes and the suggested bump, choose the version, then cut:

```sh
fishhawk release preview --repo owner/name --from v1.1.0 --to HEAD    # read-only review
fishhawk release cut --run-id <run-id> --artifact-id <artifact-id> --version v1.2.0
```

`cut` records the operator's ratified version decision as a **`release_cut`
audit entry** on the release run's chain. It **records the decision only** —
it performs **no git tag push and no GitHub write**. Nothing ships from a cut.

### The tag push is yours

After the cut, **push the release tag yourself** — this is a human git action,
not a Fishhawk operation:

```sh
git tag -s v1.2.0 -m "v1.2.0" && git push origin v1.2.0
```

Pushing the tag is what triggers the external release pipeline. Fishhawk stays
out of the git tag path by design (the delegating posture, ADR-038): the cut
endpoint ratifies the *decision*, and the operator owns the irreversible tag
push and the pipeline trigger.

### 3. `pipeline_running` → wait

The version is cut and the tag is pushed; the external release pipeline (the
deploy stage) is in flight. There is nothing to do but re-poll
`fishhawk_get_run_status` until it settles.

### 4. `awaiting_publish` → publish

The pipeline has settled (or none gates the release) and the GitHub Release for
the pushed tag exists. Publish the prepared notes to it:

```sh
fishhawk release publish --run-id <run-id> --artifact-id <artifact-id> --tag v1.2.0
```

`publish` sets the GitHub Release body to the rendered markdown and records a
**`release_published`** audit entry. It is idempotent — re-running against an
already-published Release with the same notes is a no-op.

### 5. `published` → done

A `release_published` audit entry exists; the notes are live on the GitHub
Release. The release loop is complete. Re-poll until the run resolves.

## Auth

- **preview / prepare** — authenticated (401 anonymous); `prepare` additionally
  needs `write:runs` (403 without) because it persists an artifact.
- **cut / publish** — the same write ladder as the sibling release write
  handlers (`write:runs`); a cookie session is exempt from the scope gate.

All four are new endpoints, so no existing token is tightened (the impact
inventory is empty per the AGENTS.md auth-change checklist).

## See also

- `backend/cmd/fishhawk-mcp/README.md` — the `fishhawk_release_notes` tool entry.
- `cli/README.md` — the `fishhawk release` verbs.
- `docs/api/v0.md` — the release endpoints.
- ADR-051 / [#1590](https://github.com/kuhlman-labs/fishhawk/issues/1590) — E33.5 operator release verbs.
