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
  value       = var.enable_alb ? aws_security_group.alb[0].id : ""
  description = "Empty string when enable_alb=false (bare-minimum dev)."
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

# --- ECS / ALB outputs (E13.7.2) ---

output "ecs_cluster_name" {
  value = aws_ecs_cluster.main.name
}

output "ecs_service_name" {
  value = aws_ecs_service.fishhawkd.name
}

output "task_definition_family" {
  value       = aws_ecs_task_definition.fishhawkd.family
  description = "Task definition family. The deploy workflow registers a new revision in this family per release."
}

output "alb_dns_name" {
  value       = var.enable_alb ? aws_lb.fishhawkd[0].dns_name : ""
  description = "AWS-default ALB hostname. Smoke-test against this when var.domain_name is empty. Empty when enable_alb=false (bare-minimum dev)."
}

output "alb_zone_id" {
  value       = var.enable_alb ? aws_lb.fishhawkd[0].zone_id : ""
  description = "Route 53 zone ID for ALB alias records. Empty when enable_alb=false."
}

output "fishhawkd_url" {
  description = "URL operators curl to smoke-test. When the ALB is provisioned: ALB DNS or var.domain_name. When not (bare-minimum dev): empty — reach the task at its ENI public IP via aws ecs describe-tasks."
  value = (
    var.enable_alb
    ? (var.domain_name == "" ? "http://${aws_lb.fishhawkd[0].dns_name}" : "https://${var.domain_name}")
    : ""
  )
}

# --- RDS / migrations (E13.7.3) ---

output "db_instance_address" {
  value       = aws_db_instance.main.address
  description = "RDS endpoint hostname. The task already reads this via the database_url secret; exposed here for ad-hoc psql + monitoring."
}

output "db_instance_port" {
  value = aws_db_instance.main.port
}

output "db_master_user_secret_arn" {
  value       = aws_db_instance.main.master_user_secret[0].secret_arn
  description = "AWS-managed Secrets Manager ARN holding the master password. The libpq URL in `aws_secretsmanager_secret.database_url` is assembled from this."
  sensitive   = true
}

output "migrate_task_definition_family" {
  value       = aws_ecs_task_definition.migrate.family
  description = "Task definition family for `fishhawkd migrate up`. Run via `aws ecs run-task --task-definition <family>` (latest revision) before promoting a new service revision."
}
