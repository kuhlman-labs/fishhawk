# Partial S3 backend config — the env-specific bucket / key /
# lock-table are passed at `terraform init` time:
#
#   terraform init \
#     -backend-config="bucket=$TF_STATE_BUCKET" \
#     -backend-config="key=$TF_STATE_KEY" \
#     -backend-config="dynamodb_table=$TF_LOCK_TABLE" \
#     -backend-config="region=$AWS_REGION" \
#     -backend-config="encrypt=true"
#
# CI reads these values from the active GitHub environment
# (`vars.TF_STATE_BUCKET`, `vars.TF_STATE_KEY` etc., set per
# environment via `gh api`). Operators running locally can put
# the same values in a backend.<env>.hcl file and pass it via
# `-backend-config=backend.dev.hcl`.
#
# Reasoning is in ADR-016 (#165) — Terraform state lives in S3
# with a DynamoDB table for distributed locks, bootstrapped
# out-of-band per infra/terraform/README.md. The state-store
# bucket has versioning + encryption + public-access-block
# turned on at create time; one bucket per AWS account, one
# state-file key per environment.

terraform {
  backend "s3" {}
}
