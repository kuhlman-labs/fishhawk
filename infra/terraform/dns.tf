/*
 * DNS + TLS — only provisioned when var.domain_name is non-empty.
 *
 * count = var.domain_name == "" ? 0 : 1 on every resource gives a
 * single boolean toggle: with a domain, we add an ACM cert (DNS-
 * validated against the operator's hosted zone), an alias record
 * in that zone pointing at the ALB, and an HTTPS listener that
 * presents the cert. Without a domain, this file is a no-op and
 * the ALB serves on its AWS-default hostname over HTTP only via
 * alb.tf.
 *
 * The cert lives in the same region as the ALB. Cross-region
 * cert (e.g. us-east-1 cert for a CloudFront distribution) isn't
 * a v0 concern.
 */

locals {
  # DNS + TLS resources are doubly gated — they need both a domain
  # AND an ALB to point at. enable_alb=false (bare-minimum dev)
  # disables DNS regardless of var.domain_name.
  dns_enabled        = var.domain_name != "" && var.enable_alb
  oauth_callback_url = local.dns_enabled ? "https://${var.domain_name}/v0/auth/github/callback" : ""
}

resource "aws_acm_certificate" "fishhawkd" {
  count = local.dns_enabled ? 1 : 0

  domain_name       = var.domain_name
  validation_method = "DNS"

  lifecycle {
    create_before_destroy = true
  }

  tags = {
    Name = var.domain_name
  }
}

# DNS validation record. ACM emits a CNAME we have to add to the
# zone; for_each over the cert's domain_validation_options handles
# the (possible) multi-domain case cleanly.
resource "aws_route53_record" "cert_validation" {
  for_each = {
    for dvo in flatten([
      for cert in aws_acm_certificate.fishhawkd : cert.domain_validation_options
      ]) : dvo.domain_name => {
      name   = dvo.resource_record_name
      record = dvo.resource_record_value
      type   = dvo.resource_record_type
    }
  }

  zone_id         = var.hosted_zone_id
  name            = each.value.name
  type            = each.value.type
  records         = [each.value.record]
  ttl             = 60
  allow_overwrite = true
}

resource "aws_acm_certificate_validation" "fishhawkd" {
  count = local.dns_enabled ? 1 : 0

  certificate_arn         = aws_acm_certificate.fishhawkd[0].arn
  validation_record_fqdns = [for r in aws_route53_record.cert_validation : r.fqdn]
}

# Public alias record pointing the operator's domain at the ALB.
# Alias records are AWS-only sugar over A records; they free us
# from caring about the ALB's IPs (which can change) at a small
# Route 53 query-cost discount.
resource "aws_route53_record" "fishhawkd" {
  count = local.dns_enabled ? 1 : 0

  zone_id = var.hosted_zone_id
  name    = var.domain_name
  type    = "A"

  alias {
    name                   = aws_lb.fishhawkd[0].dns_name
    zone_id                = aws_lb.fishhawkd[0].zone_id
    evaluate_target_health = true
  }
}

# HTTPS listener. Bound to the validated cert; forwards to the
# same target group the HTTP listener (in alb.tf) redirects to.
resource "aws_lb_listener" "https" {
  count = local.dns_enabled ? 1 : 0

  load_balancer_arn = aws_lb.fishhawkd[0].arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"
  certificate_arn   = aws_acm_certificate_validation.fishhawkd[0].certificate_arn

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.fishhawkd[0].arn
  }
}
