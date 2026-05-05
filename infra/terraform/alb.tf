/*
 * Application Load Balancer for fishhawkd. Sits in the two public
 * subnets, forwards :443 (and :80 redirected to :443 when a domain
 * is configured) to the Fargate task on :8080.
 *
 * Without var.domain_name set, only the HTTP listener exists and
 * it forwards directly to the target group. That's the
 * smoke-deploy posture — operators hit the AWS-default ALB DNS
 * name. Production sets domain_name + hosted_zone_id and gets the
 * cert + Route 53 record + HTTPS listener via dns.tf.
 */

resource "aws_lb" "fishhawkd" {
  name               = "${var.project}-${var.environment}"
  load_balancer_type = "application"
  security_groups    = [aws_security_group.alb.id]
  subnets            = aws_subnet.public[*].id

  # Idle timeout matches a long-poll-friendly profile; v0 doesn't
  # have long-polling endpoints but headroom is cheap.
  idle_timeout = 60

  # Enable deletion protection in production-shaped envs only;
  # leaving it always-on makes torn-down dev stacks awkward.
  enable_deletion_protection = var.environment == "prod"

  drop_invalid_header_fields = true

  tags = {
    Name = "${var.project}-${var.environment}"
  }
}

# Target group: tasks register here when the ECS service places
# them; the ALB load-balances across them.
resource "aws_lb_target_group" "fishhawkd" {
  name        = "${var.project}-${var.environment}"
  port        = 8080
  protocol    = "HTTP"
  target_type = "ip" # awsvpc network mode means tasks get ENIs
  vpc_id      = aws_vpc.main.id

  health_check {
    path                = "/healthz"
    protocol            = "HTTP"
    matcher             = "200"
    interval            = 15
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  # Deregistration delay shapes how long the LB waits for in-flight
  # requests to drain before killing a stopping task. fishhawkd's
  # graceful-shutdown story is short (no long-running connections),
  # so 30s is plenty.
  deregistration_delay = 30

  tags = {
    Name = "${var.project}-${var.environment}"
  }
}

# HTTP listener.
#
# When no domain is configured, this forwards directly to the
# target group — operators hit the ALB at the AWS-default DNS
# name. When a domain IS configured, this redirects to HTTPS;
# dns.tf adds the actual HTTPS listener.
resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.fishhawkd.arn
  port              = 80
  protocol          = "HTTP"

  default_action {
    type = var.domain_name == "" ? "forward" : "redirect"

    dynamic "forward" {
      for_each = var.domain_name == "" ? [1] : []
      content {
        target_group {
          arn = aws_lb_target_group.fishhawkd.arn
        }
      }
    }

    dynamic "redirect" {
      for_each = var.domain_name == "" ? [] : [1]
      content {
        port        = "443"
        protocol    = "HTTPS"
        status_code = "HTTP_301"
      }
    }
  }
}
