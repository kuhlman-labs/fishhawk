# fishhawk/auth

Composite action that mints a Fishhawk App installation token via GitHub Actions OIDC and outputs it for use by `actions/checkout` and the Fishhawk runner action.

Used as the first step in a Fishhawk workflow — before `actions/checkout` — so every git operation against the customer's repo (clone, fetch, push, PR creation) authenticates as the Fishhawk App. There is exactly one App-token issuance per stage, recorded in the audit log with `auth_method=oidc` and bound to the OIDC claims for repository + workflow.

## Usage

```yaml
permissions:
  id-token: write   # required for OIDC token minting
  contents: read    # actions/checkout authenticates with the App token below

steps:
  - name: Fishhawk auth
    id: fishhawk-auth
    uses: kuhlman-labs/fishhawk/auth@auth/v0.1.0
    with:
      run-id:      ${{ inputs.run_id }}
      stage-id:    ${{ inputs.stage_id }}
      backend-url: ${{ vars.FISHHAWK_BACKEND_URL }}

  - name: Checkout
    uses: actions/checkout@v6
    with:
      token: ${{ steps.fishhawk-auth.outputs.token }}

  - name: Run Fishhawk runner
    uses: kuhlman-labs/fishhawk/runner@runner/v0.1.0
    with:
      run-id:       ${{ inputs.run_id }}
      stage-id:     ${{ inputs.stage_id }}
      backend-url:  ${{ vars.FISHHAWK_BACKEND_URL }}
      github-token: ${{ steps.fishhawk-auth.outputs.token }}
      # ... other inputs ...
```

## Inputs

| Name | Required | Description |
|---|---|---|
| `run-id` | yes | Workflow run UUID supplied by the backend dispatcher. |
| `stage-id` | yes | Stage UUID for this dispatch. |
| `backend-url` | yes | Fishhawk backend base URL (e.g. `https://api.fishhawk.example.com`). |
| `oidc-audience` | no | Audience claim the backend's OIDC verifier expects. Defaults to the backend URL. |

## Outputs

| Name | Description |
|---|---|
| `token` | App installation token. Pass to `actions/checkout`'s `token:` and the runner's `github-token:`. Masked in logs via `::add-mask::`. |

## Why OIDC

GitHub Actions can mint short-lived OIDC ID tokens bound to the running workflow's identity. The Fishhawk backend verifies the token's audience + repository + workflow claims against the run row, then mints an App installation token. No long-lived secret has to live in the workflow environment.

The alternative (workflow's `GITHUB_TOKEN`) would force customers to enable the repo-level "Allow GitHub Actions to create and approve pull requests" toggle and grant `pull-requests: write` on `GITHUB_TOKEN`. Installing the Fishhawk App is the only repo-side dependency under the OIDC flow. See #201 for the design rationale.

## Token TTL and long-running stages

App installation tokens have a ~1-hour TTL. The token this action mints is used by `actions/checkout` for the initial clone; the Fishhawk runner action then mints a **second** App token immediately before its `git push` so an agent run that takes longer than ~55 minutes doesn't fail at the push step on a stale credential. The audit log records both issuances: the OIDC one (this step) and the Ed25519 one (signed by the per-run signing key right before push), each with `auth_method` recorded so the trail is unambiguous about which credential authenticated which git operation.

## Failure modes

- `OIDC env vars missing — set 'permissions: id-token: write'` — workflow doesn't have the right permissions block.
- `Backend response missing token` — the backend's `/v0/runs/{run_id}/installation-token` endpoint returned a non-token response. Inspect the backend log; common causes are the run having no `installation_id` (run was created without a webhook-attributed installation) or the OIDC verifier rejecting the claims.
- `OIDC ID token fetch returned empty value` — Actions runtime didn't return a token. Often a transient infra failure; re-run.

## See also

- [`docs/github-app/README.md`](../docs/github-app/README.md) — the App's permissions and registration flow.
- `runner/action.yml` — the Fishhawk runner action that consumes this token.
- Issue #201 — design rationale + architectural notes.
