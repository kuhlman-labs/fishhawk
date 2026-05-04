# Registering the Fishhawk GitHub App

Fishhawk runs as a GitHub App. The App provides:

- **Webhook events** that drive workflow triggers (issues labeled, comments matching `/fishhawk run`).
- **Per-installation tokens** so the backend can read `.fishhawk/workflows.yaml`, fire `workflow_dispatch`, comment on issues, open PRs.
- **OAuth user identification** for the Web UI sign-in flow (E4.2 / #49).

This directory ships:

- [`manifest.template.json`](./manifest.template.json) — the App manifest with placeholder `{{BACKEND_URL}}` markers. Templates the URLs the operator fills in for their deploy.
- [`../../scripts/render-github-app-manifest.sh`](../../scripts/render-github-app-manifest.sh) — a 30-line bash wrapper that renders the template for a given backend URL.

## Permissions

Per `MVP_SPEC.md` §5.1.5:

| Permission | Level | Purpose |
|---|---|---|
| `contents` | rw | Read `.fishhawk/workflows.yaml` from the customer's repo; push branches for the implement stage. |
| `issues` | rw | Read the originating issue's body for prompt construction; comment back with the rendered plan. |
| `pull_requests` | rw | Open PRs from the runner's pushed branch. |
| `checks` | rw | Surface stage outcomes as a check run on the PR. |
| `workflows` | w | Fire `workflow_dispatch` to invoke the runner action. |
| `members` | r | Resolve `@org/team` references in role definitions to GitHub-login allowlists for approver checks (E4.4 / #50). |
| `metadata` | r | Always granted; required for any read access. |

Webhook events:

| Event | Why |
|---|---|
| `issues` | Trigger on `labeled` with the `fishhawk` label. |
| `issue_comment` | Trigger on `created` matching `/fishhawk run`. |
| `pull_request` | Future: trigger flows on PR-side actions. |
| `push` | Future: branch-policy + spec-change detection. |
| `workflow_run` | Observe customer-side runner job state. |
| `check_run`, `check_suite` | Required-status visibility on review-stage gates. |

## Registration paths

Pick one. Manifest flow is faster and removes manual scope-typo risk; manual setup is the fallback when something in the manifest doesn't resolve cleanly (rare).

### A. Manifest flow (recommended)

GitHub's manifest flow takes a one-shot JSON payload and creates the App in your account or org. The rendered manifest carries the right scopes + events; you only fill in the App name and confirm.

1. Render the manifest with your backend's public URL:

   ```sh
   ./scripts/render-github-app-manifest.sh https://api.fishhawk.example.com > /tmp/fishhawk-app.json
   jq . /tmp/fishhawk-app.json   # optional sanity check
   ```

2. Open one of these URLs in a browser. Replace `<owner>` with your GitHub user or org name:

   - **Personal account**: `https://github.com/settings/apps/new`
   - **Organization**: `https://github.com/organizations/<owner>/settings/apps/new`

3. Submit the rendered JSON via curl to the manifest-conversion endpoint:

   ```sh
   # GitHub responds with HTML carrying a `code` you'll need next.
   # The manifest-flow URL must include `state` so GitHub redirects
   # back to your backend with the conversion code.
   curl -X POST -H "Content-Type: application/json" \
     "https://api.github.com/app-manifests/CODE/conversions"
   ```

   In practice most teams use the **GitHub-hosted manifest form**: paste the JSON from step 1, follow the redirects, accept the prompt. GitHub creates the App, generates the App ID, webhook secret, and private key, and downloads everything in one shot.

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

## See also

- `docs/MVP_SPEC.md` §5.1.5 — App component definition.
- `docs/MVP_SPEC.md` §5.4 — auth model, including OAuth via the App.
- [ADR-005 (#69)](https://github.com/kuhlman-labs/fishhawk/issues/69) — session model + bearer-token shape for the CLI surface.
- `backend/README.md` — runtime config for the App credentials and OAuth flow.
