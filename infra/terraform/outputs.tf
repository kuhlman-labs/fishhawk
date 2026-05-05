/*
 * Outputs that subsequent slices reference. Once slice 2 lands the
 * ECS service, it'll consume `private_subnet_ids` + `app_security_
 * group_id` directly inside the same root module; this file gives
 * operators a stable surface to script against (`terraform output
 * -json`) and lets the deploy workflow read role ARNs without
 * digging through state.
 */

output "vpc_id" {
  value       = aws_vpc.main.id
  description = "VPC ID."
}

output "public_subnet_ids" {
  value       = aws_subnet.public[*].id
  description = "Subnets the ALB lives in."
}

output "private_subnet_ids" {
  value       = aws_subnet.private[*].id
  description = "Subnets the Fargate tasks + RDS live in."
}

output "alb_security_group_id" {
  value = aws_security_group.alb.id
}

output "app_security_group_id" {
  value = aws_security_group.app.id
}

output "rds_security_group_id" {
  value = aws_security_group.rds.id
}

output "task_execution_role_arn" {
  value = aws_iam_role.task_execution_role.arn
}

output "task_role_arn" {
  value = aws_iam_role.task_role.arn
}

output "github_actions_deploy_role_arn" {
  value       = aws_iam_role.github_actions_deploy.arn
  description = "ARN the deploy workflow assumes via OIDC. Configure GHA secret AWS_DEPLOY_ROLE_ARN with this value."
}

output "log_group_name" {
  value = aws_cloudwatch_log_group.fishhawkd.name
}

output "secret_arns" {
  value       = local.secret_arns
  description = "Secrets Manager ARNs the task definition references in its `secrets` array."
}
