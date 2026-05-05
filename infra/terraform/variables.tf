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

# --- ECS task / service knobs (E13.7.2) ---

variable "image_tag" {
  description = "Container image tag (e.g. \"main\", \"sha-abc123\", \"v0.1.0\"). The deploy workflow (slice 4) overrides this per release; manual applies use the default."
  type        = string
  default     = "main"
}

variable "image_repository" {
  description = "Container repository for fishhawkd. Public GHCR by default — the backend-build / backend-release workflows push here."
  type        = string
  default     = "ghcr.io/kuhlman-labs/fishhawkd"
}

variable "task_cpu" {
  description = "ECS task CPU units (1024 = 1 vCPU). 256 (0.25 vCPU) is the Fargate floor and fits v0 traffic comfortably."
  type        = number
  default     = 256

  validation {
    condition     = contains([256, 512, 1024, 2048, 4096], var.task_cpu)
    error_message = "task_cpu must be one of the Fargate-supported values: 256, 512, 1024, 2048, 4096."
  }
}

variable "task_memory" {
  description = "ECS task memory in MiB. Fargate requires the cpu/memory pair to match a supported combination; 512 with cpu=256 is the floor."
  type        = number
  default     = 512
}

variable "task_desired_count" {
  description = "Number of running task replicas. v0 ships with 1; bump to 2 for HA across the two private subnets."
  type        = number
  default     = 1
}

# --- DNS + TLS (E13.7.2; optional) ---
#
# When domain_name + hosted_zone_id are both supplied the stack
# provisions an ACM cert (DNS validation), a Route 53 alias record
# pointing at the ALB, and an HTTPS listener. Otherwise the ALB
# serves on its AWS-default hostname over HTTP only — fine for a
# smoke deploy, not for production.

variable "domain_name" {
  description = "Public hostname for the ALB (e.g. \"fishhawk.example.dev\"). Leave empty to skip the ACM cert + Route 53 record + HTTPS listener; HTTP-only on the ALB's default hostname."
  type        = string
  default     = ""
}

variable "hosted_zone_id" {
  description = "Route 53 hosted zone ID owning domain_name. Required when domain_name is set; ignored otherwise."
  type        = string
  default     = ""

  validation {
    condition     = var.hosted_zone_id == "" || can(regex("^Z[A-Z0-9]+$", var.hosted_zone_id))
    error_message = "hosted_zone_id must look like a Route 53 zone id (Z followed by uppercase alphanumerics)."
  }
}
