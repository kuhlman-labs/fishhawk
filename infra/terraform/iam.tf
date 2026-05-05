/*
 * Three roles, three trust models:
 *
 *   - task_execution_role: assumed by ECS itself (ecs-tasks).
 *       Pulls the image from GHCR-via-pull-through-cache or ECR,
 *       writes container logs to CloudWatch, reads secrets from
 *       Secrets Manager. The ECS task definition references this
 *       role's ARN in `executionRoleArn`.
 *
 *   - task_role: assumed by the running container.
 *       fishhawkd's runtime AWS API surface — currently empty
 *       beyond the implicit S3 access for trace storage and
 *       Secrets Manager for refreshed lookups. Keep narrow; ADR-009
 *       calls this out as the trust boundary.
 *
 *   - github_actions_deploy: assumed by the deploy workflow via
 *       OIDC (no long-lived AWS keys per ADR-009 #73). Permissions
 *       cover what the deploy workflow needs and nothing more —
 *       ECS describe + update-service + run-task (for migrations).
 *       Slice 4 wires the GitHub Actions side.
 */

data "aws_caller_identity" "current" {}

data "aws_region" "current" {}

data "aws_iam_policy_document" "ecs_tasks_assume" {
  statement {
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "task_execution_role" {
  name               = "${var.project}-${var.environment}-task-execution"
  description        = "ECS-side role: pulls images, writes logs, reads secrets for the task definition's `secrets` array."
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
}

# Standard execution-role baseline: ECR pull + CloudWatch Logs.
resource "aws_iam_role_policy_attachment" "task_execution_managed" {
  role       = aws_iam_role.task_execution_role.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

# Inline policy for Secrets Manager access. Limited to the specific
# ARNs declared in secrets.tf — adding a new secret requires editing
# both files, which is the kind of friction that catches "I added
# a secret but the task can't read it" before deploy.
data "aws_iam_policy_document" "task_execution_secrets" {
  statement {
    actions   = ["secretsmanager:GetSecretValue"]
    resources = local.secret_arns
  }
}

resource "aws_iam_policy" "task_execution_secrets" {
  name        = "${var.project}-${var.environment}-task-execution-secrets"
  description = "Allow the ECS execution role to fetch fishhawkd's runtime secrets at task-start time."
  policy      = data.aws_iam_policy_document.task_execution_secrets.json
}

resource "aws_iam_role_policy_attachment" "task_execution_secrets" {
  role       = aws_iam_role.task_execution_role.name
  policy_arn = aws_iam_policy.task_execution_secrets.arn
}

# --- task_role -----------------------------------------------------

resource "aws_iam_role" "task_role" {
  name               = "${var.project}-${var.environment}-task"
  description        = "Container-side role for fishhawkd. Empty by default; subsequent slices attach S3 + KMS as the trace-store and signing-key paths come online."
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
}

# --- github_actions_deploy -----------------------------------------

# GitHub's OIDC provider is account-scoped. Multiple repos in the same
# account share the provider; create it once if it doesn't already
# exist. The thumbprints come from GitHub's documentation; rotate via
# `aws iam update-open-id-connect-provider` if GitHub publishes new
# CA fingerprints. Adopting the resource into Terraform when it
# already exists: `terraform import aws_iam_openid_connect_provider.github
# arn:aws:iam::ACCOUNT:oidc-provider/token.actions.githubusercontent.com`.
resource "aws_iam_openid_connect_provider" "github" {
  url            = "https://token.actions.githubusercontent.com"
  client_id_list = ["sts.amazonaws.com"]
  thumbprint_list = [
    "6938fd4d98bab03faadb97b34396831e3780aea1",
    "1c58a3a8518e8759bf075b76b750d4f2df264fcd",
  ]

  tags = {
    Name = "github-oidc"
  }
}

data "aws_iam_policy_document" "github_actions_assume" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.github.arn]
    }

    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }

    # Tightened in slice 4 (E13.7.4): only the main branch and
    # backend release tags can assume this role. Pull-request
    # contexts (sub = "repo:.../pull/N/merge") and feature
    # branches (sub = "repo:.../ref:refs/heads/<branch>") match
    # neither pattern and so cannot deploy.
    #
    # workflow_dispatch from main-branch context produces
    # "repo:<repo>:ref:refs/heads/main", which the first pattern
    # covers — so manual rollback works from the Actions UI.
    condition {
      test     = "StringLike"
      variable = "token.actions.githubusercontent.com:sub"
      values = [
        "repo:${var.github_repo}:ref:refs/heads/main",
        "repo:${var.github_repo}:ref:refs/tags/backend/v*",
      ]
    }
  }
}

resource "aws_iam_role" "github_actions_deploy" {
  name               = "${var.project}-${var.environment}-gha-deploy"
  description        = "Assumed by GitHub Actions via OIDC for the fishhawkd deploy workflow. Permissions cover ECS describe + update + run-task for migrations."
  assume_role_policy = data.aws_iam_policy_document.github_actions_assume.json
}

# Deploy permissions:
#  - ecs:DescribeServices / DescribeTaskDefinition / RegisterTaskDefinition for `aws ecs update-service`
#  - ecs:UpdateService for the actual deploy
#  - ecs:RunTask + iam:PassRole for the migration task (slice 4)
#  - logs:DescribeLogGroups / FilterLogEvents for surfacing failures in the workflow output
data "aws_iam_policy_document" "github_actions_deploy" {
  statement {
    actions = [
      "ecs:DescribeServices",
      "ecs:DescribeTasks",
      "ecs:DescribeTaskDefinition",
      "ecs:ListTasks",
      "ecs:RegisterTaskDefinition",
      "ecs:UpdateService",
      "ecs:RunTask",
      "ecs:StopTask",
    ]
    resources = ["*"]
  }

  # PassRole is required to point ECS at the task / execution roles.
  # Limited to those two ARNs so a compromised workflow can't pivot.
  statement {
    actions = ["iam:PassRole"]
    resources = [
      aws_iam_role.task_role.arn,
      aws_iam_role.task_execution_role.arn,
    ]
    condition {
      test     = "StringEquals"
      variable = "iam:PassedToService"
      values   = ["ecs-tasks.amazonaws.com"]
    }
  }

  statement {
    actions = [
      "logs:DescribeLogGroups",
      "logs:DescribeLogStreams",
      "logs:FilterLogEvents",
      "logs:GetLogEvents",
    ]
    resources = ["*"]
  }
}

resource "aws_iam_policy" "github_actions_deploy" {
  name        = "${var.project}-${var.environment}-gha-deploy"
  description = "Permissions for the fishhawkd GitHub Actions deploy workflow."
  policy      = data.aws_iam_policy_document.github_actions_deploy.json
}

resource "aws_iam_role_policy_attachment" "github_actions_deploy" {
  role       = aws_iam_role.github_actions_deploy.name
  policy_arn = aws_iam_policy.github_actions_deploy.arn
}
