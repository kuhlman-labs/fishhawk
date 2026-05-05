variable "environment" {
  description = "Environment name (e.g. dev, staging, prod). Used in resource names + tag values."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,30}$", var.environment))
    error_message = "environment must be lowercase alphanumeric + dashes, 2-31 chars."
  }
}

variable "region" {
  description = "AWS region for all resources in this stack."
  type        = string
  default     = "us-east-1"
}

variable "project" {
  description = "Project name; used as a prefix on resource names + tag value."
  type        = string
  default     = "fishhawk"
}

variable "vpc_cidr" {
  description = "CIDR block for the VPC."
  type        = string
  default     = "10.0.0.0/16"
}

variable "availability_zones" {
  description = "Two AZs in var.region. The ALB spans both; Fargate + RDS live in private subnets in both. NAT gateway lives in just one (cost optimization for v0; multi-AZ NAT is a v1+ change)."
  type        = list(string)

  validation {
    condition     = length(var.availability_zones) == 2
    error_message = "availability_zones must contain exactly 2 entries."
  }
}

variable "github_repo" {
  description = "GitHub repo (owner/name) the GHA OIDC role trusts. Subject claims must match this for AssumeRoleWithWebIdentity to succeed."
  type        = string
  default     = "kuhlman-labs/fishhawk"
}

variable "log_retention_days" {
  description = "Retention for the fishhawkd CloudWatch Logs group. Production keeps a month; dev environments can drop to 7."
  type        = number
  default     = 30
}
