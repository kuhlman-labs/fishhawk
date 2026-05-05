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

3. Drop the credentials into a local `.env` (gitignored — already covered by `.env*` in `.gitignore`):

   ```sh
   FISHHAWKD_GITHUB_APP_ID=123456
   FISHHAWKD_GITHUB_APP_PRIVATE_KEY_FILE=/abs/path/fishhawk-app.private-key.pem
   FISHHAWKD_GITHUB_WEBHOOK_SECRET=whatever-you-set
   FISHHAWKD_OAUTH_CLIENT_ID=Iv1.xxxx
   FISHHAWKD_OAUTH_CLIENT_SECRET=xxxx
   FISHHAWKD_OAUTH_CALLBACK_URL=http://localhost:8080/v0/auth/github/callback
   ```

4. Run the backend with the env loaded:

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
