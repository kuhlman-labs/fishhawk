# CloudWatch Logs group for fishhawkd's stdout/stderr. The task
# definition (slice 2) wires the awslogs driver here. slog's JSON
# output flows into structured fields in CloudWatch Logs Insights
# without further transformation.
resource "aws_cloudwatch_log_group" "fishhawkd" {
  name              = "/aws/ecs/${var.project}-${var.environment}"
  retention_in_days = var.log_retention_days

  tags = {
    Name = "${var.project}-${var.environment}-logs"
  }
}
