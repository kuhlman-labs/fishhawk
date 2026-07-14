# backend/internal/workmgmt/github

GitHub work-item provider (`github_projects`): issue filing, board placement on Projects v2, board-state transitions, and the epic-children query (see `backend/internal/workmgmt/README.md` for the capability contracts).

## User-owned Projects v2 board placement (#1114)

Why a GitHub App installation token can't board Project #7:

- `FISHHAWKD_PROJECTS_TOKEN` (`--projects-token`, `serve.go` → `cfg.GitHub.ProjectsToken`) is an optional user PAT/UAT carrying the **`project`** scope. A GitHub App installation token has no user-projects permission, so it cannot reach a personal-account Projects v2 (the `Could not resolve to a ProjectV2 with the number 7` errors).
- The provider's `placeOnBoard` opts the three board-placement GraphQL calls (`ProjectFields` / `AddProjectItem` / `SetProjectItemSingleSelect`) into the projects token via `githubclient.WithProjectsToken(ctx)` **only when `proj.OwnerType=="user"`**.
- `doGraphQL` honors the flag only when `Client.ProjectsToken` is non-empty, otherwise it falls back to the installation token — so issue creation (REST) + epic sub-issue linking (`AddSubIssue`, repo-scoped) stay on the installation token, and an unset token preserves the #1107 best-effort `boarded:false` degradation unchanged.
- The token is a secret: never logged or traced (startup logs `projects_token_configured` presence only), never included in an error message.
- Code: `backend/internal/githubclient/{client.go,projects.go}` + `backend/internal/workmgmt/github/provider.go`.
