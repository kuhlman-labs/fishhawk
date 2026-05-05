# Fishhawk infra (Terraform)

Per [ADR-016](https://github.com/kuhlman-labs/fishhawk/issues/165) — Terraform manages all hosted infrastructure for `fishhawkd`. This directory has, as of E13.7.2:

- **Foundation** ([#148](https://github.com/kuhlman-labs/fishhawk/issues/148)) — VPC + subnets, security groups, IAM roles, Secrets Manager skeletons, CloudWatch log group.
- **ECS service + ALB** ([#166](https://github.com/kuhlman-labs/fishhawk/issues/166)) — Fargate task definition pointing at the GHCR image, ECS service across both private subnets, Application Load Balancer with HTTP listener (HTTPS + ACM + Route 53 alias gated on `domain_name`).

What's **not** here yet (subsequent slices):

- RDS Postgres + migration runner ([#167](https://github.com/kuhlman-labs/fishhawk/issues/167))
- `backend-deploy.yml` workflow that updates the task definition revision per release ([#168](https://github.com/kuhlman-labs/fishhawk/issues/168))

## Prerequisites

- Terraform `~> 1.5` (Homebrew: `brew install terraform`)
- An AWS account with admin access for the bootstrap (the deploy workflow uses a narrowly-scoped OIDC role created here, but the *bootstrap* needs IAM + S3 + DynamoDB write access)
- AWS CLI configured (`aws configure`) for the bootstrap commands below

## Bootstrap (one-time per account)

Terraform's S3 backend can't store its own state — the chicken-and-egg of "the state store stores its own state." Create the state bucket and lock table once, by hand:

```sh
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
REGION=us-east-1

# Versioned state bucket
aws s3api create-bucket \
  --bucket fishhawk-tfstate-${ACCOUNT_ID} \
  --region ${REGION} \
  $([ "${REGION}" != "us-east-1" ] && echo "--create-bucket-configuration LocationConstraint=${REGION}")
aws s3api put-bucket-versioning \
  --bucket fishhawk-tfstate-${ACCOUNT_ID} \
  --versioning-configuration Status=Enabled
aws s3api put-bucket-encryption \
  --bucket fishhawk-tfstate-${ACCOUNT_ID} \
  --server-side-encryption-configuration '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"}}]}'
aws s3api put-public-access-block \
  --bucket fishhawk-tfstate-${ACCOUNT_ID} \
  --public-access-block-configuration "BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true"

# Lock table
aws dynamodb create-table \
  --region ${REGION} \
  --table-name fishhawk-tfstate-lock \
  --attribute-definitions AttributeName=LockID,AttributeType=S \
  --key-schema AttributeName=LockID,KeyType=HASH \
  --billing-mode PAY_PER_REQUEST
```

Then create the local `backend.tf`:

```sh
cp backend.tf.example backend.tf
# Edit backend.tf: replace <account-id> with $ACCOUNT_ID, set <env>
# (use "prod" for the first production stack).
```

Create the per-environment vars file:

```sh
cp prod.tfvars.example prod.tfvars
# Edit prod.tfvars to taste (region, AZs, github_repo).
```

## Apply

```sh
terraform init
terraform plan -var-file=prod.tfvars -out=plan.tfplan
terraform apply plan.tfplan
```

Expected resources after slice 2 (~40):

Foundation (~25):
- 1 VPC + 1 IGW + 1 NAT gateway + 1 EIP
- 4 subnets (2 public, 2 private)
- 3 route tables + 4 associations
- 3 security groups (ALB, app, RDS)
- 4 Secrets Manager entries
- 3 IAM roles (task execution, task, GHA OIDC) + their policies
- 1 OIDC provider (token.actions.githubusercontent.com)
- 1 CloudWatch Log group

Slice 2 (~15 more):
- 1 ECS cluster + capacity-provider config
- 1 ECS task definition + 1 ECS service
- 1 ALB + 1 target group
- 1 HTTP listener (forward when no domain; redirect-to-HTTPS otherwise)
- When `domain_name` set: 1 ACM cert + 2 Route 53 records (validation + alias) + 1 ACM validation + 1 HTTPS listener

## Post-apply manual steps

The Secrets Manager entries are created **empty**. Populate them with real values before slice 2 brings up the ECS service:

```sh
aws secretsmanager put-secret-value \
  --secret-id fishhawk/prod/database_url \
  --secret-string 'postgres://USER:PASS@HOST:5432/fishhawk?sslmode=require'

aws secretsmanager put-secret-value \
  --secret-id fishhawk/prod/github_app_private_key \
  --secret-string "$(cat path/to/app-private-key.pem)"

aws secretsmanager put-secret-value \
  --secret-id fishhawk/prod/github_webhook_secret \
  --secret-string "$(openssl rand -hex 32)"

aws secretsmanager put-secret-value \
  --secret-id fishhawk/prod/oauth_client_secret \
  --secret-string '<github-oauth-app-client-secret>'
```

Configure the GitHub App's webhook URL + secret to match (the URL points at the ALB after slice 2 lands).

## Smoke testing the deploy

Without a domain, the ALB serves on its AWS-default hostname over plain HTTP — fine for confirming the task is running:

```sh
URL=$(terraform output -raw fishhawkd_url)
curl -sf "$URL/healthz"
# {"status":"ok","version":"…"}
```

Cold-start time is ~30s — Fargate task placement, image pull from GHCR, binary boot. The ECS service's circuit breaker auto-rolls-back if a new task definition fails health checks too many times in a row.

To follow logs:

```sh
aws logs tail /aws/ecs/fishhawk-prod --follow
```

## Cost notes (us-east-1, on-demand, v0 traffic)

| Resource | Approx /mo |
|---|---|
| NAT gateway (single AZ, ~10 GB egress) | ~$35 |
| EIP (attached) | $0 |
| Subnets, route tables, IGW | $0 |
| Secrets Manager (4 secrets) | ~$1.60 |
| CloudWatch Logs (30d retention, ~5 GB ingest) | ~$3 |
| ECS Fargate (1 × 256 CPU / 512 MB / 24×7) | ~$9 |
| ALB (always-on + ~5 GB) | ~$18 |
| ACM cert + Route 53 (when configured) | ~$1 |
| **Total through slice 2** | **~$67** |

RDS lands in slice 3 and adds the bulk of the remaining cost (~$15 for `db.t4g.micro`).

## See also

- [ADR-009 (#73)](https://github.com/kuhlman-labs/fishhawk/issues/73) — hosted deploy target choice (ECS Fargate)
- [ADR-016 (#165)](https://github.com/kuhlman-labs/fishhawk/issues/165) — IaC tool choice (Terraform)
- [E13.4 (#61)](https://github.com/kuhlman-labs/fishhawk/issues/61) — secrets management strategy
- `docs/ARCHITECTURE.md` — "Where to look" entries for the deployed components
