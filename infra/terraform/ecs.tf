/*
 * ECS cluster + task definition + service for fishhawkd.
 *
 * The image is the GHCR multi-stage distroless build from
 * .github/workflows/backend-build.yml. Image:tag is wired through
 * var.image_tag so the deploy workflow (slice 4) can update the
 * task definition revision per release without re-applying the
 * rest of the stack.
 *
 * Networking: tasks run in the private subnets with the app
 * security group attached. assign_public_ip = false — outbound
 * traffic goes through the NAT gateway in network.tf.
 *
 * Health check: ELB-driven via the ALB target group's path /healthz.
 * The grace period gives the binary time to dial the DB + S3 +
 * GitHub on cold start before the ALB starts marking targets
 * unhealthy.
 *
 * Deployment: rolling, with circuit breaker enabled so a bad
 * task definition rolls back automatically (saves a manual
 * "aws ecs update-service --task-definition <prior>" step).
 */

resource "aws_ecs_cluster" "main" {
  name = "${var.project}-${var.environment}"

  setting {
    name  = "containerInsights"
    value = "enabled"
  }

  tags = {
    Name = "${var.project}-${var.environment}"
  }
}

resource "aws_ecs_cluster_capacity_providers" "main" {
  cluster_name = aws_ecs_cluster.main.name

  capacity_providers = ["FARGATE", "FARGATE_SPOT"]

  default_capacity_provider_strategy {
    capacity_provider = "FARGATE"
    weight            = 1
    base              = 1
  }
}

# Task definition. The container's `secrets` array references
# Secrets Manager entries from secrets.tf by ARN — ECS resolves
# them at task-start time and exposes them as env vars to the
# container without ever writing the values to the task
# definition itself. `environment` carries the non-secret config.
locals {
  fishhawkd_container_name = "fishhawkd"

  fishhawkd_image = "${var.image_repository}:${var.image_tag}"

  # Task secrets injected as env vars. Add a new secret here
  # whenever you add an aws_secretsmanager_secret in secrets.tf.
  fishhawkd_secret_env = [
    {
      name      = "FISHHAWKD_DATABASE_URL"
      valueFrom = aws_secretsmanager_secret.database_url.arn
    },
    {
      name      = "FISHHAWKD_GITHUB_WEBHOOK_SECRET"
      valueFrom = aws_secretsmanager_secret.github_webhook_secret.arn
    },
    {
      name      = "FISHHAWKD_OAUTH_CLIENT_SECRET"
      valueFrom = aws_secretsmanager_secret.oauth_client_secret.arn
    },
    # github_app_private_key is a multi-line PEM. fishhawkd today
    # reads it via a *file path* (--github-app-private-key-file),
    # so injecting it as an env var requires either a backend
    # change to accept the value directly, or a sidecar that writes
    # it to a shared volume. Tracked under #163's follow-ups; for
    # now the GitHub App endpoints respond 503 until the operator
    # provides the key via a bind-mounted secret.
  ]

  # Non-secret environment. `FISHHAWKD_ADDR` matches the Dockerfile
  # CMD; explicit here so a future port change is one place. The
  # OAuth callback URL needs to match the registered OAuth App.
  fishhawkd_env = [
    { name = "FISHHAWKD_ADDR", value = ":8080" },
    { name = "FISHHAWKD_OAUTH_CALLBACK_URL", value = local.oauth_callback_url },
  ]
}

resource "aws_ecs_task_definition" "fishhawkd" {
  family                   = "${var.project}-${var.environment}"
  network_mode             = "awsvpc"
  requires_compatibilities = ["FARGATE"]
  cpu                      = var.task_cpu
  memory                   = var.task_memory
  execution_role_arn       = aws_iam_role.task_execution_role.arn
  task_role_arn            = aws_iam_role.task_role.arn

  container_definitions = jsonencode([
    {
      name      = local.fishhawkd_container_name
      image     = local.fishhawkd_image
      essential = true

      portMappings = [
        {
          containerPort = 8080
          hostPort      = 8080
          protocol      = "tcp"
        }
      ]

      environment = local.fishhawkd_env
      secrets     = local.fishhawkd_secret_env

      # awslogs driver streams stdout/stderr (slog JSON) into
      # the CloudWatch group from logs.tf. awslogs-stream-prefix
      # tags each task's stream so multi-replica deploys are
      # debuggable.
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          awslogs-group         = aws_cloudwatch_log_group.fishhawkd.name
          awslogs-region        = var.region
          awslogs-stream-prefix = "ecs"
        }
      }

      # Container-level health check. ALB target group also probes
      # /healthz; this catches the case where the container starts
      # but the binary hasn't bound :8080 yet (rolling deploys
      # would otherwise count it healthy too early). The image is
      # distroless, so we exec the binary directly with a special
      # one-shot health subcommand... except we don't ship one.
      # Skip the container check; rely on the ALB target group.
    }
  ])

  tags = {
    Name = "${var.project}-${var.environment}"
  }
}

resource "aws_ecs_service" "fishhawkd" {
  name            = "${var.project}-${var.environment}"
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.fishhawkd.arn
  desired_count   = var.task_desired_count
  launch_type     = "FARGATE"

  # Wait this long after a new task starts before the ALB target
  # group counts unhealthy starts against the rollout. fishhawkd
  # boots quickly; 60s is generous enough for cold-start DB +
  # GitHub auth + S3 region resolution.
  health_check_grace_period_seconds = 60

  network_configuration {
    subnets          = aws_subnet.private[*].id
    security_groups  = [aws_security_group.app.id]
    assign_public_ip = false
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.fishhawkd.arn
    container_name   = local.fishhawkd_container_name
    container_port   = 8080
  }

  # Circuit breaker auto-rolls-back when too many consecutive task
  # starts fail health checks; saves a manual rollback step on a
  # bad release. enable=true + rollback=true is the AWS-recommended
  # production posture.
  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  deployment_controller {
    type = "ECS"
  }

  # Force the dependency from listener to service so HTTPS / HTTP
  # is up before the service tries to register targets.
  depends_on = [
    aws_lb_listener.http,
  ]

  lifecycle {
    # The deploy workflow updates task_definition (new revision per
    # release); we don't want `terraform apply` to revert that
    # between deploys. desired_count is similarly subject to manual
    # ops adjustment.
    ignore_changes = [task_definition, desired_count]
  }

  tags = {
    Name = "${var.project}-${var.environment}"
  }
}
