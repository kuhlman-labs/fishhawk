/*
 * Dedicated task definition for `fishhawkd migrate up`.
 *
 * One-shot — operator (slice 4: the deploy workflow) launches it
 * via `aws ecs run-task --launch-type FARGATE …` before swapping
 * the service revision. The container exits when the migration
 * runner is done; ECS reports the exit code in `describe-tasks`.
 *
 * Same image, same secrets, same env. The only differences from
 * the serve task definition (ecs.tf) are:
 *
 *   - `command = ["migrate", "up"]` overrides the Dockerfile CMD
 *   - `essential = true` so a non-zero exit fails the run-task
 *     immediately rather than restarting the container
 *   - sized smaller (256/512 is already the floor; explicit so a
 *     future serve-task bump doesn't drag migrations along)
 *
 * The runbook for invoking this from the CLI is in README.md;
 * slice 4 (#168) wraps the same call in the deploy workflow.
 */

resource "aws_ecs_task_definition" "migrate" {
  family                   = "${var.project}-${var.environment}-migrate"
  network_mode             = "awsvpc"
  requires_compatibilities = ["FARGATE"]
  cpu                      = 256
  memory                   = 512
  execution_role_arn       = aws_iam_role.task_execution_role.arn
  task_role_arn            = aws_iam_role.task_role.arn

  container_definitions = jsonencode([
    {
      name      = "migrate"
      image     = local.fishhawkd_image
      essential = true

      command = ["migrate", "up"]

      environment = local.fishhawkd_env
      secrets     = local.fishhawkd_secret_env

      logConfiguration = {
        logDriver = "awslogs"
        options = {
          awslogs-group         = aws_cloudwatch_log_group.fishhawkd.name
          awslogs-region        = var.region
          awslogs-stream-prefix = "migrate"
        }
      }
    }
  ])

  tags = {
    Name = "${var.project}-${var.environment}-migrate"
  }
}
