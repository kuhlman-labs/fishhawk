/*
 * Security groups — the trust chain is ALB → app → RDS.
 *
 *   internet → ALB(443/80) → app(8080) → rds(5432)
 *
 * Each SG only allows ingress from the next hop. Egress is the
 * default "anything outbound" because the app needs to talk to
 * GitHub, Anthropic, S3, and Secrets Manager; tightening egress is
 * a v1+ defense-in-depth pass.
 */

# ALB-facing SG: HTTPS from anywhere. HTTP redirect handled by the
# ALB listener (added in slice 2); we still allow :80 here so the
# redirect works.
resource "aws_security_group" "alb" {
  name_prefix = "${var.project}-${var.environment}-alb-"
  description = "fishhawkd ALB ingress"
  vpc_id      = aws_vpc.main.id

  ingress {
    description = "HTTPS from internet"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  ingress {
    description = "HTTP from internet (redirect to HTTPS at the listener level)"
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    description = "All outbound (forwards to app SG via TCP only in practice)"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name = "${var.project}-${var.environment}-alb"
  }

  lifecycle {
    create_before_destroy = true
  }
}

# App-facing SG: only the ALB can reach :8080. Bound to the Fargate
# task ENI in slice 2.
resource "aws_security_group" "app" {
  name_prefix = "${var.project}-${var.environment}-app-"
  description = "fishhawkd Fargate task ingress"
  vpc_id      = aws_vpc.main.id

  ingress {
    description     = "HTTP from ALB"
    from_port       = 8080
    to_port         = 8080
    protocol        = "tcp"
    security_groups = [aws_security_group.alb.id]
  }

  egress {
    description = "All outbound (GitHub, Anthropic, S3, Secrets Manager)"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name = "${var.project}-${var.environment}-app"
  }

  lifecycle {
    create_before_destroy = true
  }
}

# RDS SG: only the app SG can reach Postgres. RDS instance lands in
# slice 3.
resource "aws_security_group" "rds" {
  name_prefix = "${var.project}-${var.environment}-rds-"
  description = "fishhawkd Postgres ingress"
  vpc_id      = aws_vpc.main.id

  ingress {
    description     = "Postgres from app"
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = [aws_security_group.app.id]
  }

  # No egress rule needed for v0 — RDS Postgres doesn't initiate
  # outbound connections in our flow.

  tags = {
    Name = "${var.project}-${var.environment}-rds"
  }

  lifecycle {
    create_before_destroy = true
  }
}
