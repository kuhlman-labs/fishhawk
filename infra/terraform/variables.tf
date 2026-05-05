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

# --- Dev cost-cutting toggles ---
#
# A bare-minimum dev deploy turns both off. NAT gateway is the
# single biggest line item (~$32/mo); the ALB is the next
# (~$18/mo). Flipping both saves ~$50/mo, leaving RDS as the
# remaining floor at ~$15/mo.
#
#  - enable_nat_gateway=false → ECS tasks lose private outbound.
#    They run in the public subnets with assign_public_ip=true,
#    talking to GHCR / Anthropic / Secrets Manager directly via
#    the IGW. Acceptable for dev; never for prod.
#  - enable_alb=false        → no public ingress at all. The
#    container has its own ENI public IP (when NAT is also off);
#    operators reach it via aws ecs describe-tasks → eni public IP.
#    Or stand up a temporary tunnel (cloudflared, ngrok) for
#    interactive testing.

variable "enable_nat_gateway" {
  description = "Provision the single-AZ NAT gateway + EIP. Off for bare-minimum dev: ECS tasks land in the public subnets with task_assign_public_ip=true instead. Saves ~$32/mo."
  type        = bool
  default     = true
}

variable "enable_alb" {
  description = "Provision the Application Load Balancer + target group + HTTP/HTTPS listeners. Off for bare-minimum dev. Saves ~$18/mo."
  type        = bool
  default     = true
}

variable "task_assign_public_ip" {
  description = "Place ECS tasks in the public subnets with public IPs. Required when enable_nat_gateway=false (no other path to the internet). The app security group's ingress widens to 0.0.0.0/0 in this mode so an operator can hit /healthz on the task ENI directly. Dev only — production should always run with this off."
  type        = bool
  default     = false

  # Cross-variable validation isn't allowed in Terraform 1.5
  # variable blocks (it landed in 1.9). The expected pairing —
  # task_assign_public_ip=true ⇔ enable_nat_gateway=false — is
  # documented inline in dev.tfvars.example and enforced via the
  # null_resource precondition in the network module below.
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

# --- RDS knobs (E13.7.3) ---

variable "db_engine_version" {
  description = "Postgres major.minor for the RDS instance. AWS gates instance class availability on engine version; bump deliberately when AWS publishes a new minor."
  type        = string
  default     = "16.4"
}

variable "db_instance_class" {
  description = "RDS instance class. db.t4g.micro is the cheapest Graviton tier (~$15/mo); production should bump to db.t4g.small or db.m6g.large depending on traffic."
  type        = string
  default     = "db.t4g.micro"
}

variable "db_allocated_storage" {
  description = "Provisioned storage in GB. Auto-scales up to db_max_allocated_storage when full (no downtime; the auto-scaler bumps in 10% chunks)."
  type        = number
  default     = 20
}

variable "db_max_allocated_storage" {
  description = "Auto-scaling ceiling in GB. 100 keeps us well clear of v0 traffic without runaway risk on a misbehaving migration."
  type        = number
  default     = 100
}

variable "db_backup_retention_days" {
  description = "Daily-snapshot retention. 7 days covers \"oh shit\" recovery without paying for archival storage; production may bump to 30."
  type        = number
  default     = 7
}

variable "db_multi_az" {
  description = "Enable Multi-AZ failover. Doubles RDS cost and flips us to synchronous replication. v0 ships single-AZ; bump for production once a partner relies on the deployment."
  type        = bool
  default     = false
}

variable "db_deletion_protection" {
  description = "Block accidental `terraform destroy` of the RDS instance. Prod should set this to true and disable explicitly when truly tearing down."
  type        = bool
  default     = false
}
