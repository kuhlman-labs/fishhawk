# Registering the Fishhawk GitHub App

Fishhawk runs as a GitHub App. The App provides:

- **Webhook events** that drive workflow triggers (issues labeled, comments matching `/fishhawk run`, `/fishhawk approve`, `/fishhawk reject`).
- **Per-installation tokens** so the backend can read `.fishhawk/workflows.yaml`, fire `workflow_dispatch`, comment on issues, open PRs.
- **OAuth user identification** for the Web UI sign-in flow (E4.2 / #49).

This directory ships:

- [`manifest.template.json`](./manifest.template.json) — the App manifest with placeholder `{{BACKEND_URL}}` markers. Templates the URLs the operator fills in for their deploy.
- [`../../scripts/render-github-app-manifest.sh`](../../scripts/render-github-app-manifest.sh) — bash wrapper that renders the template for a given backend URL + webhook URL.

## Permissions

Per `MVP_SPEC.md` §5.1.5:

| Permission | Level | Purpose |
|---|---|---|
| `actions` | rw | Fire `workflow_dispatch` to invoke the runner action (`POST /repos/.../actions/workflows/{file}/dispatches`). Distinct from `workflows`, which only covers editing workflow files. |
| `contents` | rw | Read `.fishhawk/workflows.yaml` from the customer's repo; push branches for the implement stage; update GitHub Release bodies + attach release-notes assets for the publish integration (E33.3 / #1588 — see the note below). |
| `issues` | rw | Read the originating issue's body for prompt construction; comment back with the rendered plan. |
| `pull_requests` | rw | Open PRs from the runner's pushed branch. |
| `checks` | rw | Surface stage outcomes as a check run on the PR. |
| `workflows` | w | Edit `.github/workflows/*.yml` (e.g. install-time provisioning of the customer's `fishhawk.yml`). Does NOT cover the dispatch endpoint — see `actions` above. |
| `members` | r | Resolve `@org/team` references in role definitions to GitHub-login allowlists for approver checks (E4.4 / #50). |
| `metadata` | r | Always granted; required for any read access. |
| `administration` | r | Read branch protection + rulesets to derive the required-checks list (ADR-017 / #249). |
| `organization_projects` | rw | Board work items on the installing account's **org-owned** Projects v2 via the installation token (`fishhawk_file_issue` / `fishhawk_report_product_issue`, #1116). **Does NOT cover user-owned Projects v2** (e.g. Project #7) — no App permission can; those need a UAT/PAT (#1114). |

**Releases are covered by `contents`, not a new permission (E33.3 / #1588,
ADR-051).** The release-publish integration (`POST /v0/releases/publish`) reads
a Release by tag, PATCHes its body, and deletes/uploads the release-notes asset
via the GitHub Releases REST endpoints. Per GitHub's "Permissions required for
GitHub Apps" reference, those endpoints require only `contents: write`, which
the App already holds (it pushes run branches) — confirmed on #1588's permission
inventory by the repo owner. So E33.3 adds NO permission and existing installs
need no re-consent; the auth-change impact inventory is empty.

Webhook events:

| Event | Why |
|---|---|
| `issues` | Trigger on `labeled` with the `fishhawk` label. |
| `issue_comment` | Trigger on `created` matching `/fishhawk run`, `/fishhawk approve`, or `/fishhawk reject`. |
| `pull_request` | Future: trigger flows on PR-side actions. |
| `push` | Future: branch-policy + spec-change detection. |
| `workflow_run` | Observe customer-side runner job state. |
| `check_run`, `check_suite` | Required-status visibility on review-stage gates. |
| `branch_protection_rule`, `repository_ruleset` | Acknowledged so a future cache layer can invalidate the per-run protection snapshot (ADR-017 / #251). v0 reads protection on every run-create — no cache to bust today. |
| `installation`, `installation_repositories` | Auto-onboarding (ADR-048 / E29.7): when the App is installed (`installation.created`) or repos are added to an existing install (`installation_repositories.added`), the backend opens a reviewable scaffold PR per newly-added repo. Other actions (deleted / suspend / removed) are acknowledged and skipped. |

## Local development

GitHub can't reach `localhost`, and OAuth callback URLs are matched exactly — so local dev splits into three modes. Pick the simplest that unblocks the work in front of you:

| Mode | What works | What doesn't | Setup |
|---|---|---|---|
| **A. No App** | API + Web UI in dev mode (warnings are non-fatal; OAuth and webhook endpoints respond 503) | UI sign-in; receiving GitHub events | None — `make dev-backend` is enough |
| **B. App with OAuth, no webhooks** | UI sign-in; manual run dispatch via the CLI | Receiving GitHub events (issues / PRs) | Register an App with a `localhost` callback; ignore webhooks |
| **C. Full App with tunneled webhooks** | Everything | — | Register an App; expose `:8080` via smee.io or cloudflared |

Most local UI work fits in Mode B. Reach for Mode C only when iterating on the webhook receiver itself.

### Mode A — no App

Run `make dev-backend` without setting any of the `FISHHAWKD_GITHUB_*` or `FISHHAWKD_OAUTH_*` env vars. The startup logs will warn that:

- `/webhooks/github` responds 503,
- `/v0/auth/github/*` responds 503,
- the role resolver is disabled (the approval handler accepts any authenticated subject — fine for local testing).

Everything else — runs, plans, audit log, retries — works against the local stack.

### Mode B — App with OAuth, no webhooks

1. Register an App on a personal account: <https://github.com/settings/apps/new>. Match the permissions and events table above. For the URLs:

   - **Callback URL**: `http://localhost:8080/v0/auth/github/callback`
   - **Webhook URL**: any placeholder (e.g. `http://localhost:9999/unused`); uncheck **Active**.

2. On the App's settings page, generate a webhook secret, generate and download a private key (`.pem`), and note the App ID + OAuth Client ID + Client secret.

3. **Enable Device Flow**: on the same settings page, under **General**, check **Enable Device Flow** and click **Update application**. The manifest schema (see the callout under "Registration paths" below) cannot express this, so it must be turned on by hand — and Mode B's CLI-driven `fishhawk token login` is the device flow, so skipping this step fails every login attempt with `device_flow_disabled`.

4. Drop the credentials into a local `.env` (gitignored — already covered by `.env*` in `.gitignore`):

   ```sh
   FISHHAWKD_GITHUB_APP_ID=123456
   FISHHAWKD_GITHUB_APP_PRIVATE_KEY_FILE=/abs/path/fishhawk-app.private-key.pem
   FISHHAWKD_GITHUB_WEBHOOK_SECRET=whatever-you-set
   FISHHAWKD_OAUTH_CLIENT_ID=Iv1.xxxx
   FISHHAWKD_OAUTH_CLIENT_SECRET=xxxx
   FISHHAWKD_OAUTH_CALLBACK_URL=http://localhost:8080/v0/auth/github/callback
   ```

5. Run the backend with the env loaded:

   ```sh
   set -a; source .env; set +a
   make dev-backend
   ```

The frontend dev server proxies `/v0` to `:8080`, so `http://localhost:5173` → "sign in" will round-trip through GitHub OAuth back to the UI.

### Mode C — full App with tunneled webhooks

Add a tunnel in front of `:8080`. Two common options:

- **smee.io** (zero install): visit <https://smee.io/new> for a forwarding URL, then run a client that forwards to localhost:

  ```sh
  npx smee-client -u https://smee.io/abc123 -t http://localhost:8080/webhooks/github
  ```

  Set the App's **Webhook URL** to the smee URL. Leave the OAuth callback on `http://localhost:8080/v0/auth/github/callback` (browsers handle the redirect; only GitHub-originated webhook traffic needs the tunnel).

- **cloudflared** (real HTTPS hostname for the duration of the tunnel):

  ```sh
  brew install cloudflared
  cloudflared tunnel --url http://localhost:8080
  ```

  Use the printed `https://*.trycloudflare.com` URL for both the App's webhook URL **and** OAuth callback URL — and set `FISHHAWKD_OAUTH_CALLBACK_URL` to match.

Set the same env vars as Mode B. After the tunnel is up, `Redeliver` a webhook from the App's **Advanced** tab to verify the round-trip.

### Faster Mode B / C: drive registration through the backend

Once `fishhawkd` is running, the manifest flow + credential fetch can be done end-to-end without leaving the browser. Visit:

```
http://localhost:8080/v0/auth/github/manifest-flow-start?backend_url=http://localhost:8080&webhook_url=https://smee.io/<id>
```

The backend mints state, the page auto-submits to GitHub, GitHub creates the App and redirects back, and the callback page renders the App ID + secrets + PEM in one go. Copy the `.env` block out before closing the tab.

For Mode B (OAuth-only), use a placeholder webhook URL (e.g. `https://smee.io/anything-unique` even if you don't run the smee client). For Mode C, use the smee URL you'll actually forward.

## Registration paths

Pick one. Manifest flow is faster and removes manual scope-typo risk; manual setup is the fallback when something in the manifest doesn't resolve cleanly (rare).

> **Enable Device Flow — required, all paths.** GitHub's App manifest parameter set (verified 2026-07-12 against GitHub's manifest-flow docs) has no device-flow key, so **no registration path below — A, A', or B — can turn this on for you.** After the App is created, go to its settings page → **General** → check **Enable Device Flow** → **Update application**. Until that box is checked, every `fishhawk token login` fails with `device_flow_disabled`.

### A. Manifest flow via the backend (recommended)

`fishhawkd` ships two endpoints (E4.7) that drive the whole flow end-to-end:

1. **`GET /v0/auth/github/manifest-flow-start`** mints a state value, sets a short-lived cookie, and returns an auto-submitting form pointing at GitHub.
2. **`GET /v0/auth/github/manifest-callback`** verifies state, exchanges the one-shot conversion `code` with `api.github.com`, and renders an HTML page with the App ID, OAuth client ID + secret, webhook secret, and PEM. **Secrets are shown once.** Copy them into `.env` (local dev) or your secrets backend before closing the tab.

To start the flow, hit:

```
http://localhost:8080/v0/auth/github/manifest-flow-start?backend_url=<URL>&webhook_url=<URL>
```

Required query parameters:

- `backend_url` — absolute base URL of `fishhawkd` (e.g. `http://localhost:8080`, `https://api.fishhawk.example.com`).
- `webhook_url` — destination GitHub will deliver webhooks to. In production, this is `<backend_url>/webhooks/github`. For local dev, use a [smee.io](https://smee.io/new) forwarding URL (GitHub can't reach `localhost`).

Optional:

- `owner=<user-or-org>` — register on an org instead of the personal account.
- `name=<App name>` — override the default "Fishhawk".

The page POSTs to GitHub on load. After you confirm the App's name on GitHub's side, GitHub redirects back to `<backend_url>/v0/auth/github/manifest-callback?code=…&state=…`, the backend exchanges the code, and you land on the credentials page.

### A'. Manifest rendering by hand

If you'd rather build the manifest yourself (e.g. to register without the backend running, or to script it from CI), the same template is exposed via a CLI helper:

```sh
./scripts/render-github-app-manifest.sh \
  https://api.fishhawk.example.com \
  https://api.fishhawk.example.com/webhooks/github \
  > /tmp/fishhawk-app.json
```

You'd then POST that JSON to `https://github.com/settings/apps/new` (or `/organizations/<owner>/settings/apps/new`) yourself, and convert the resulting `code` against `https://api.github.com/app-manifests/<code>/conversions` within ten minutes. The backend flow above does both steps for you.

### B. Manual setup

If you'd rather click through the form:

1. **Personal**: <https://github.com/settings/apps/new>. **Org**: replace `settings` with `organizations/<owner>/settings`.
2. Set the homepage URL, callback URL (`{{BACKEND_URL}}/v0/auth/github/callback`), and webhook URL (`{{BACKEND_URL}}/webhooks/github`).
3. **Generate a webhook secret** (`openssl rand -hex 32`) — you'll set this on the backend too.
4. Match the permissions in the table above.
5. Subscribe to the events listed above.
6. Set "Where can this GitHub App be installed?" to **Only on this account** for v0 / staging; flip to **Any account** when you're ready for the Marketplace listing (E10).
7. Click **Create GitHub App**.
8. From the App's settings page:
   - Note the **App ID** (numeric).
   - Click **Generate a private key**. Download the `.pem`.
   - **Optional but recommended**: configure the App to also issue OAuth user-tokens by checking "Request user authorization (OAuth) during installation" and saving the resulting **Client ID** + **Client secret**. The Web UI sign-in flow (E4.2) uses these.
   - **Enable Device Flow**: under **General**, check **Enable Device Flow** and click **Update application**. Skipping this fails every `fishhawk token login` with `device_flow_disabled` — see the callout above.

## Configuring the backend

Once registered, supply credentials to `fishhawkd`:

```sh
export FISHHAWKD_GITHUB_APP_ID="123456"
export FISHHAWKD_GITHUB_APP_PRIVATE_KEY_FILE="/path/to/fishhawk-app.private-key.pem"
export FISHHAWKD_GITHUB_WEBHOOK_SECRET="..."   # the secret you set in step 3
# Web UI OAuth (optional; only when the App also acts as an OAuth provider):
export FISHHAWKD_OAUTH_CLIENT_ID="Iv1...."
export FISHHAWKD_OAUTH_CLIENT_SECRET="..."
export FISHHAWKD_OAUTH_CALLBACK_URL="https://api.fishhawk.example.com/v0/auth/github/callback"

fishhawkd serve
```

Per ADR-005 (#69) and E13.4 (#61) the production secrets path is AWS Secrets Manager, not env files; use env files only for local dev.

## Installing on a customer repo

Customer-side: visit `https://github.com/apps/fishhawk` (or the App's settings page during private testing), pick the repo to install on, and grant access. Their installation triggers the backend's webhook receiver and is ready to run.

### Auto-onboarding: the App-PR scaffold (ADR-048 / E29.7)

Installing the App does more than register the webhook: the backend opens a **reviewable scaffold pull request** on each newly-added repo so a customer needs zero local setup to start. On `installation.created` (and `installation_repositories.added` for repos added later), the backend authors a single commit through the GitHub Git Data API — no working tree — that seeds four files, and opens a PR from the `fishhawk/onboarding` branch into the repo's default branch:

- `.fishhawk/workflows.yaml` — the workflow spec, seeded from the **medium** autonomy preset. The reviewer changes the tier in the PR before merging if they want more or less automation.
- `AGENTS.md` — the canonical, cross-agent instruction file, carrying Fishhawk's marker-delimited managed block.
- `CLAUDE.md` — a bridge file importing `AGENTS.md` so Claude Code picks up the same instructions.
- `.github/workflows/fishhawk.yml` — the customer-side execution workflow, referencing the **published** `kuhlman-labs/fishhawk/runner` + `kuhlman-labs/fishhawk/auth` actions (not local paths). Committing this file is why the App needs `workflows: write` (already granted).

The human is the author/reviewer of record (autonomy:low): the scaffold lands as a PR they review and merge, never a direct push to the default branch. The content is byte-identical to what `fishhawk init` generates for the same preset.

Idempotency: the scaffolder skips a repo that already carries `.fishhawk/workflows.yaml` (already onboarded) and treats an already-open scaffold PR as success. If a prior attempt left a stale `fishhawk/onboarding` branch, it is **force-updated** to the freshly-generated scaffold commit so the PR always reflects the current scaffold. Scaffolding is best-effort per repo — one repo's failure logs and moves on without failing the webhook.

Subscribing to `installation` / `installation_repositories` is a manifest/registration concern: it takes effect only for freshly created or updated App registrations. An existing install must re-accept (or re-register) to receive these events.

### Making the audit gate block merges

Fishhawk publishes a `fishhawk_audit_complete` Check Run on every PR's head commit (#231) once the backend has computed the run's audit-completeness state. The check is informational on its own — to make it actually block the merge button, customers add it as a **Required status check** in branch protection:

1. Wait for at least one Fishhawk run on a PR so GitHub registers the check name. (Required-check selectors only show names GitHub has previously seen on the branch.)
2. Repo **Settings → Branches**.
3. Add or edit a branch protection rule for `main` (or whatever your default is).
4. Tick **Require status checks to pass before merging**.
5. Search for `fishhawk_audit_complete` and add it. Optionally add `ci_pass` and any other check names from your workflow spec's `blocking_checks`.
6. Save.

From this point, GitHub itself refuses the merge until Fishhawk reports `success`. The "Details" link on the check on github.com routes back to the run page in Fishhawk via the operator's configured `FISHHAWKD_EXTERNAL_URL`.

### Re-installing after a permissions bump

When the App's permission set changes (e.g. a new `checks: write` requirement), GitHub flags the installation as needing review and existing installations fall back to the old permission set until the customer accepts the new ones. Reinstall via the App's installation page (**Configure** → **Save**) to pick up the new permissions.

Two recent bumps require re-consent:

- **#1116** added `organization_projects: write` so the installation token can board work items on the installing account's **org-owned** Projects v2 (consumed by `fishhawk_file_issue` / `fishhawk_report_product_issue`). Existing installs must re-accept before board placement works; until then those tools degrade to `boarded:false`. Caveat: this permission reaches org-owned Projects v2 only — user-owned Projects v2 (e.g. Project #7) are out of reach of any App permission and need a UAT/PAT (#1114), so re-consenting does not by itself fix board placement on a user-owned board.
- **ADR-017 / #249** added `administration: read` so the backend can read branch protection + rulesets at run-create time. Existing installs must re-accept before required-checks derivation will work; the run will fall back to an empty required-checks list otherwise.

**Installing the App is the only repo-side dependency.** Specifically, customers do **not** need to:

- Enable **Settings → Actions → General → Workflow permissions → "Allow GitHub Actions to create and approve pull requests"**. The runner uses the App's installation token for push and PR creation (per #197), not the workflow's `GITHUB_TOKEN`.
- Grant write permissions to the workflow's `GITHUB_TOKEN`. The shipped `.github/workflows/fishhawk.yml` declares `permissions: contents: read` and that's enough — `actions/checkout` reads the repo, the runner does its writes via the App.

The trade-off: PRs are authored by the Fishhawk App rather than the customer's automation account, which is the correct attribution per `BRAND_FOUNDATIONS.md` §5 ("Fishhawk holds the record").

## See also

- `docs/MVP_SPEC.md` §5.1.5 — App component definition.
- `docs/MVP_SPEC.md` §5.4 — auth model, including OAuth via the App.
- [ADR-005 (#69)](https://github.com/kuhlman-labs/fishhawk/issues/69) — session model + bearer-token shape for the CLI surface.
- `backend/README.md` — runtime config for the App credentials and OAuth flow.
