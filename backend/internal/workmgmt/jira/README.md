# backend/internal/workmgmt/jira

Jira Cloud work-item provider (`provider: jira`) — the concrete second provider (#1094), deferred from #1005.

## Filing (`provider.go`)

- `File` maps a resolved `workmgmt.ProviderRequest` onto `backend/internal/jiraclient` (Jira Cloud REST v3, HTTP Basic).
- It creates the issue first — the durable/fatal step. Labels and the optional `Relations.ParentEpic` are mapped at create time; the parent epic maps to the team-managed `fields.parent` reference.
- It then **best-effort** (#1107) transitions the issue to `BoardPlacement.Status` — a transition failure records `BoardingError` and leaves `Boarded=false` rather than discarding the issue.
- Issue type maps through the conventions `jira.issue_types` (default: title-cased canonical type).
- `CreatedItem.Number` is the key's numeric suffix; `URL` carries the full key (browse URL).

## Configuration: server-side env, not repo config

The instance base URL + credentials come from `FISHHAWKD_JIRA_BASE_URL` / `FISHHAWKD_JIRA_EMAIL` / `FISHHAWKD_JIRA_API_TOKEN` (matching the `FISHHAWKD_PROJECTS_TOKEN` single-instance precedent; secrets cannot live in a checked-in repo config), constructed into a single `*jiraclient.Client` in `serve.go`.

The per-repo `jira` conventions block selects only the project (`project_key` + optional `issue_types`). `Target.Jira` (populated from `conv.Jira` in `server/workitems.go`) carries the connection to the provider.

## Capability gaps and deferrals

- The board-state `Transitioner` capability (#1012) is intentionally NOT implemented for jira in v0 — the board-sync hook type-asserts it and yields a no-op move.
- **Deferred:** classic-project epic-link custom fields — v0 supports only the team-managed `fields.parent` reference.
