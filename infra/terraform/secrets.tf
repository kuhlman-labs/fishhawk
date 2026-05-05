/*
 * Secrets Manager entries for fishhawkd's runtime config. Terraform
 * creates the slots; the operator populates the *values* out-of-band
 * (`aws secretsmanager put-secret-value …` or the console). This
 * keeps secret material out of the Terraform state file.
 *
 * Each secret name is `<project>/<env>/<key>` so cross-environment
 * cross-account scans (e.g. AWS Config) bucket cleanly. The task
 * definition (slice 2) references these by ARN in its `secrets`
 * array; the execution role's IAM policy (iam.tf) grants
 * GetSecretValue on every ARN in this file.
 *
 * Adding a new secret here:
 *   1. Add an aws_secretsmanager_secret resource below.
 *   2. Append its ARN to the secret_arns local at the bottom.
 *   3. Update the task definition's `secrets` array (slice 2).
 */

resource "aws_secretsmanager_secret" "database_url" {
  name        = "${var.project}/${var.environment}/database_url"
  description = "Postgres connection string for fishhawkd. Format: postgres://user:pass@host:5432/db?sslmode=require"

  tags = {
    Name = "${var.project}/${var.environment}/database_url"
  }
}

resource "aws_secretsmanager_secret" "github_app_private_key" {
  name        = "${var.project}/${var.environment}/github_app_private_key"
  description = "PEM-encoded RSA private key for the Fishhawk GitHub App. Copy from the App's settings page after registration."

  tags = {
    Name = "${var.project}/${var.environment}/github_app_private_key"
  }
}

resource "aws_secretsmanager_secret" "github_webhook_secret" {
  name        = "${var.project}/${var.environment}/github_webhook_secret"
  description = "Shared secret for HMAC-validating GitHub webhook deliveries. Random 32-byte hex string; configure the same value in the GitHub App's webhook settings."

  tags = {
    Name = "${var.project}/${var.environment}/github_webhook_secret"
  }
}

resource "aws_secretsmanager_secret" "oauth_client_secret" {
  name        = "${var.project}/${var.environment}/oauth_client_secret"
  description = "GitHub OAuth App client_secret for the /v0/auth/github/* sign-in flow. Copy from the OAuth App's settings page."

  tags = {
    Name = "${var.project}/${var.environment}/oauth_client_secret"
  }
}

# Aggregate for the task execution role's IAM policy. Append new
# ARNs here when adding secrets above.
locals {
  secret_arns = [
    aws_secretsmanager_secret.database_url.arn,
    aws_secretsmanager_secret.github_app_private_key.arn,
    aws_secretsmanager_secret.github_webhook_secret.arn,
    aws_secretsmanager_secret.oauth_client_secret.arn,
  ]
}
