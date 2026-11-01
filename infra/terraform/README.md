# Azure hosted pilot

Authority: operator-approved [#4902](https://github.com/kuhlman-labs/fishhawk/issues/4902), 2026-11-01. Azure replaces the never-deployed AWS resources. Preserve ADR-073's Kubernetes/Helm convergence; revise its EKS and two-environment choices. Historical ADR records remain the original decisions; this implementation issue records the approved pilot amendment. Terraform remains the IaC tool (ADR-016).

## Topology and ownership

- `bootstrap/`: operator-owned local state; creates state storage, pilot resource group, protected `infra` GitHub OIDC identity and role assignments. Back up this state securely. Do not recreate it on every apply.
- Root: ONE shared `pilot.tfstate`, not a workspace per namespace. Owns VM/K3s, networking, two PostgreSQL servers, per-environment vaults/storage/SAS/deploy identities, budget alerts. All shared infrastructure changes affect prod and require the protected `infra` environment; no automatic infrastructure apply on main.
- Application releases: Helm in `fishhawk-dev`, `fishhawk-staging`, `fishhawk-prod`. Namespace boundaries are installed by the operator, never writable by deployment identities. Namespace credentials and application data are distinct. Dev/staging share a DB server with distinct databases/roles; prod has its own server/subnet.
- Kubernetes, TLS, namespace policy, secret/DB bootstrap: `deploy/azure/configure.py`, executed by the operator on the VM. No application DB or trace data is stored on the VM.
- Application release configuration/state: independently owned Helm releases and namespace credentials; neither one environment's deploy identity nor Helm can alter shared infrastructure. Terraform state contains generated credentials: use Azure AD authentication, Blob leases, versioning, limited readers, and private handling of plan files.

No resources have been provisioned by this PR. The bootstrap is deliberately explicit: domain/DNS ownership, SKU capacity, existing GitHub Apps, and live secrets cannot be inferred from a subscription login.

## Budget and operating limits

East US retail PAYG estimate, USD, 730 hours, checked 2026-11-01 using Azure Retail Prices API:

| Resource | Monthly estimate |
|---|---:|
| One B2ms Linux VM, 2 vCPU / 8 GiB, $0.0832/hour | $60.74 |
| Two B1ms PostgreSQL servers, $0.017/hour each | $24.82 |
| 32 GiB DB storage each, $0.115/GiB-month | $7.36 |
| VM Standard SSD + public IP | $7–10 |
| Low-volume Blob, Key Vault, DNS, state storage, diagnostics, egress | $5–10 |
| Total before tax | $105–113 |

This is a low-traffic pilot estimate, not a quota or an Azure quote. Model tokens, paid GitHub Actions, domain registration and support plans are outside it. Budget emails at 50/80/100% do NOT stop spending; the resource-group budget also excludes the separate small state-storage group. No NAT Gateway, Azure Load Balancer, Bastion, paid managed monitoring workspace, ACR, or AKS management charge is provisioned. Ingress binds the VM's ports 80/443; the instance public IP provides explicit outbound connectivity. Logs remain bounded node/container logs; collect incident evidence before recycling the node.

Single VM = shared failure domain and planned downtime for updates. No HA claim. PostgreSQL burstable instances have CPU-credit and connection limits and are not Microsoft's recommended production tier; this is an explicit pilot compromise. Alert on CPU credits, VM memory/disk, PostgreSQL CPU/credits/connections/storage and certificate/SAS expiration before customer onboarding. Expand the budget before sustained CPU, database headroom, availability or stronger isolation requires it. Agent runners, repository builds and CI remain off this VM. Avoid reservation commitments until utilization is measured.

Live commissioning and operational alert/restore evidence are tracked in [#4903](https://github.com/kuhlman-labs/fishhawk/issues/4903); complete that checklist before customer onboarding.

## Prerequisites

- Azure CLI logged into the intended subscription; Terraform 1.17.1 (CI pin; minimum 1.5.7), SSH and an operator public key. Bootstrap requires role-assignment privileges. Use the checked-in provider lock files.
- Both GHCR packages (`fishhawkd` and the newly published `fishhawk-web`) must be publicly pullable before deployment; set package visibility explicitly after first publication. This pilot does not provision private-registry credentials.
- Choose globally unique `name` (4–12 lowercase alphanumeric characters), region, an owned base domain, an ACME contact and budget email. Verify regional B2ms/B1ms availability, quota and current prices before apply.
- Register resource providers once: `Microsoft.Compute`, `Microsoft.Network`, `Microsoft.Storage`, `Microsoft.KeyVault`, `Microsoft.ManagedIdentity`, `Microsoft.DBforPostgreSQL`, `Microsoft.Consumption` (`az provider register --namespace <name> --wait`). Provider auto-registration is disabled so CI needs no subscription-wide registration privileges.
- Discover an exact Ubuntu image using `az vm image list --publisher Canonical --offer ubuntu-24_04-lts --sku server --location eastus --all`; set `image_version`. Never deploy the example `0.0.0` placeholder.
- `admin_cidrs` restrict SSH. `deployment_cidrs` must include the actual Kubernetes client source IPs. Prefer fixed-egress runners. Standard GitHub-hosted runners have changing egress: either maintain their published CIDRs or explicitly accept a publicly reachable, authenticated TLS port 6443 by setting `0.0.0.0/0`. There is no anonymous API access. The server certificate includes the static public IP; kubeconfigs embed the CA, never skip verification.

## Bootstrap and plan

Use a new directory/state; do not try migrating an AWS state file into these resources. If old AWS state exists, inventory and archive it first.

```sh
terraform -chdir=infra/terraform/bootstrap init
terraform -chdir=infra/terraform/bootstrap plan \
  -var='subscription_id=<subscription>' -var='name=<unique-prefix>' -out=bootstrap.tfplan
terraform -chdir=infra/terraform/bootstrap apply bootstrap.tfplan
# Securely back up bootstrap/terraform.tfstate and remove the sensitive plan.

cp infra/terraform/pilot.tfvars.example infra/terraform/pilot.tfvars
# Fill every operator value; set budget_start to this month's first day.
# SAS start should be slightly in the past; expiry at most 90 days later.
terraform -chdir=infra/terraform init \
  -backend-config='storage_account_name=<prefix>tfstate' \
  -backend-config=container_name=tfstate -backend-config=key=pilot.tfstate \
  -backend-config=use_azuread_auth=true
terraform -chdir=infra/terraform plan -var-file=pilot.tfvars -out=pilot.tfplan
# Review resource types, region, SKUs, identity scopes and costs before applying.
terraform -chdir=infra/terraform apply pilot.tfplan
terraform -chdir=infra/terraform output -json
```

Azure RBAC propagation can delay the first Key Vault/state operation. Wait for role propagation and retry the failed apply; do not widen permissions. The protected infrastructure identity has Contributor, RBAC Administrator and Secrets Officer only on the pilot group, plus state-container access in the state group. It is an administrative identity; application jobs never receive it.

Bootstrap PostgreSQL defaults are PG16, 32 GiB, 7-day nonprod and 14-day prod PITR retention, private networking, no HA. Destruction of DBs and trace accounts is blocked in Terraform. Resizing is a reviewed shared-infra change. No hard-coded provider credentials or app secrets are committed.

## Secrets, namespace bootstrap, DNS and TLS

Create three separate GitHub App registrations/installations, webhook secrets and OAuth clients. Configure each callback as `https://<environment-host>/v0/auth/github/callback` and webhook as `https://<environment-host>/webhooks/github`. Never install the dev/staging App into production repositories.

In EACH `<prefix>-<env>` vault, populate the following secret names from secure local files using `az keyvault secret set --vault-name ... --name ... --file ... --output none`:

- `github-app-private-key`: PEM from that environment's GitHub App.
- `github-webhook-secret`, `oauth-client-secret`, `anthropic-api-key`.

Terraform already writes `database-url`, `database-password`, `database-admin-url`, and `azure-storage-connection-string`. The last is a container SAS granting read/create/write/list, WITHOUT delete; storage accounts are distinct by environment. Versioning + soft delete preserve overwritten/deleted objects, but this is not compliance-mode immutable retention. Account keys remain inside Terraform/provider state, not application pods. SAS rotation is mandatory before `sas_expiry`.

Point three DNS A records (e.g. dev/staging/app) at `public_ip`. Copy the trusted checked-out configuration script to the VM, wait for cloud-init, then run it as root. It reads vaults using the VM identity; no Azure CLI or long-lived cloud credential is installed on the node. Example:

```sh
scp deploy/azure/configure.py fishhawk@<public-ip>:/home/fishhawk/configure.py
ssh fishhawk@<public-ip> 'sudo cloud-init status --wait'
umask 077
ssh fishhawk@<public-ip> 'sudo python3 /home/fishhawk/configure.py <prefix> <public-ip> <acme-email>' > kubeconfigs.json
# This file contains credentials. Never print it or upload it as a CI artifact.
```

The script installs pinned cert-manager, the ACME issuer, restricted namespaces, quotas, deploy RBAC, default-deny policies, DB owner roles, application Secrets and scoped deployment tokens. Re-running syncs secret rotations and rotates deploy tokens (invalidating old tokens); serialize with deployments and complete the vault update below before permitting deploys again. The node identity can read all environment vaults, so node/root compromise crosses environments; network policy blocks pod access to IMDS and other environment DB subnets. Namespace isolation is not host isolation.

Write each JSON kubeconfig to its matching vault immediately:

```sh
for env in dev staging prod; do
  jq --arg env "$env" '.[$env]' kubeconfigs.json > kubeconfig.json
  az keyvault secret set --vault-name "<prefix>-$env" --name deploy-kubeconfig --file kubeconfig.json --output none
done
rm -f kubeconfig.json kubeconfigs.json
```

Tokens are Kubernetes service-account credentials, scoped to one namespace; they are NOT cluster-admin. Azure OIDC deploy identities can read only their `deploy-kubeconfig` secret, not arbitrary vault secrets. Anyone able to deploy code into an environment can access that environment's app secrets through that code. Rotate Kubernetes tokens at least quarterly and on personnel/runner compromise. No role permits editing namespaces, RBAC, network policy, quota, or privileged pods.

## GitHub environments and variables

Create `infra`, `dev`, `staging`, `prod` environments. ALL must use custom branch policies allowing exactly the `main` branch (no tags/wildcards). `infra` and `prod` require reviewers with self-review disabled. Protection settings are checked live BEFORE requesting OIDC; absent or weaker settings fail closed. The operator permits GitHub admin bypass for `infra` and `prod`; an administrator may explicitly bypass the environment approval gate, including for a self-initiated run. This does not disable the workflow's main-only and protection-configuration checks.

| Location | Variables |
|---|---|
| Every environment | `AZURE_SUBSCRIPTION_ID`, `AZURE_TENANT_ID`, `AZURE_CLIENT_ID` (matching environment identity output) |
| `infra` | `TF_STATE_ACCOUNT`; `TF_VARS_JSON` containing the reviewed non-secret root inputs as JSON |
| Each app environment | `AZURE_KEY_VAULT`, `APP_HOST`, `FISHHAWK_GITHUB_APP_ID`, `OAUTH_CLIENT_ID` |
| Repository | Set `AZURE_HOSTING_ENABLED=true` ONLY after all bootstrap steps and permissions are verified |

Only public values belong in GitHub variables. SSH public keys are public; DB passwords, PEMs, model keys and kubeconfigs are not. Enable the opt-in flag only after smoke-testing the initial deployment path.

## Release and rollback

1. A successful main CI gate builds and cosign-signs backend and frontend images. The `hosted-release` artifact binds their digests, source commit, CI run ID, Helm chart and values hashes. Frontend uses same-origin paths; no environment API URL is baked into its image.
2. `Hosted Deploy` auto-deploys eligible main bundles to dev after CI completes. A stale queued auto-deploy refuses to replace a newer main checkout. Commits that did not build a release skip deployment.
3. Dispatch `Hosted Deploy` on main, environment `staging`, with that successful CI `build_run_id`. It verifies source provenance, signatures, hashes, migration hooks, readiness, HTTPS, health SHA and both running image digests. Success uploads a `staging-receipt` artifact.
4. Dispatch the SAME workflow on main, environment `prod`, with the SAME `build_run_id` and the successful staging deployment's `staging_run_id`. Protected approval precedes OIDC. A receipt from another release/environment/run fails. No rebuild occurs during promotion. Artifacts expire after 90 days; promote within that window.

Helm 4.2.4 uses `--rollback-on-failure --wait` (the successor to Helm 3's `--atomic`). Server/worker Deployments use Recreate to avoid duplicate singleton workers; a brief outage is intentional. Migration hooks run before application upgrade and must remain backward-compatible with the previous version. Readiness failure triggers Helm rollback; a failed post-deploy smoke check explicitly rolls back to the prior deployed revision. First install has no previous revision and is retained for diagnosis. Database migrations are NEVER automatically reversed. Do not deploy a destructive migration under the assumption that application rollback restores the schema.

Application frontend and API use one HTTPS host: `/` serves SPA, `/v0`, `/webhooks`, `/healthz`, `/mcp`, `/.well-known`, `/oauth` route to fishhawkd. cert-manager owns the TLS Secret; HTTP port 80 is for ACME validation. Cloud build/main image publishing remains gated by the existing full CI rules; the dogfood `deploy.yml` stub is not silently repurposed.

## Recovery and maintenance

- Node failure: preserve evidence if possible; rebuild VM from pinned image/K3s, restore namespace boundaries and secrets with configure.py, rotate kubeconfigs, redeploy the last successful immutable release. Application state is in PostgreSQL/Blob. Terraform owns static IP/DNS inputs; verify IP and TLS after replacement. A fresh cluster does not recover Helm history; use recorded release artifacts.
- DB incident: use Azure PostgreSQL PITR to a NEW server (never overwrite the only recovery source), verify it privately, update Terraform/import and vault URLs, sync secrets, redeploy. Restore nonprod first as a drill; measure RPO/RTO before customer onboarding. DB owner roles must stay isolated; a shared nonprod restore affects dev and staging together.
- Trace incident: restore blob version/soft-deleted object using a separate operator identity. Application SAS has no delete permission. Keep DB references and trace objects consistent; a DB backup alone is not complete recovery.
- Rotate SAS: update explicit start/expiry in the reviewed infra inputs, apply, run configure.py, store the rotated kubeconfigs, then redeploy environments. Restart pods after secret updates: an existing Secret's contents are not reflected in Helm's values checksum. Alert ahead of expiry; do not leave this to an expired-auth incident.
- Patch: schedule downtime, check active runs/review workers, update pinned K3s/OS/cert-manager versions through PRs. VM platform patching may also require reboot. Verify TLS, network policies, secrets and application health afterward. Do not run untrusted CI jobs on this VM.
- Growth: sustained load or customer uptime requires larger non-burstable DBs and an increased budget; move prod to its own AKS cluster when isolation/HA is needed. The application chart and external data services carry forward.

## Checks

When changing provider versions, run `terraform providers lock -platform=darwin_arm64 -platform=linux_amd64 -platform=linux_arm64` in BOTH Terraform roots and commit both lockfiles. CI initializes with `-lockfile=readonly`; provider package hashes must cover its Linux platform as well as the operator's platform. Reproduce Linux validation with Terraform 1.17.1 against a source-only copy, without state or Azure credentials.

`terraform fmt -check -recursive infra/terraform`; initialize/validate both roots without a backend; `python deploy/azure/test_hosting.py` with PyYAML 6.0.3 and Helm 4.2.4. The reusable infra-check job owns these checks and is included in CI Pass. Azure Blob SDK tests live in `backend/internal/tracestore/azure_test.go`. Full repository verification remains `scripts/verify-change`. Static validation does not prove subscription permissions, cloud-init, Azure provisioning, ACME issuance, or live restore behavior; those are first-deploy acceptance checks.
