# backend/internal/onboarding

Auto-opens a scaffold PR on GitHub App install (ADR-048 / E29.7).

## App-PR onboarding scaffold

`ScaffoldFiles(preset)` assembles the four scaffold files, byte-identical to what `fishhawk init` generates:

- `.fishhawk/workflows.yaml` ← `spec.PresetBytes`
- `AGENTS.md` ← `bridge.ManagedBlock`
- `CLAUDE.md` ← `bridge.ImportLine`
- `.github/workflows/fishhawk.yml` ← embedded `templates/fishhawk.yml`, referencing the PUBLISHED `kuhlman-labs/fishhawk/runner`+`auth` actions

`Scaffolder.OpenScaffoldPR` authors a single atomic commit through the GitHub **Git Data API** (create-tree with inline content → create-commit → create-ref → open PR) with no working tree.

- Idempotent: skips a repo already carrying `.fishhawk/workflows.yaml`; `ErrPullRequestExists` is success; a stale `fishhawk/onboarding` branch is **force-updated** to the fresh scaffold commit via `ForceUpdateRef`, not left stale.
- Default preset is **medium**; the human is author/reviewer of record via the PR (autonomy:low).
- Driven by the webhook dispatcher's `installation` / `installation_repositories` branch (`matchInstallation` → `MatchActionScaffold` → `Dispatcher.handleInstallation`, best-effort per repo, nil-Scaffolder safe skip; wired in `serve.go`).
- New githubclient Git Data methods (`GetRepository`, `GetCommit`, `CreateTree`, `CreateCommit`) live in `backend/internal/githubclient/gitdata.go`.
