variable "subscription_id" { type = string }
variable "name" {
  type        = string
  description = "Globally unique lowercase prefix; used for storage and Key Vault names."
  validation {
    condition     = can(regex("^[a-z][a-z0-9]{3,11}$", var.name))
    error_message = "Use 4–12 lowercase alphanumeric characters, beginning with a letter."
  }
}
variable "location" { default = "eastus" }
variable "github_repository" { default = "kuhlman-labs/fishhawk" }
variable "ssh_public_key" { type = string }
variable "admin_cidrs" {
  type        = list(string)
  description = "Operator source CIDRs for SSH. Never use 0.0.0.0/0."
  validation {
    condition     = length(var.admin_cidrs) > 0 && alltrue([for cidr in var.admin_cidrs : can(cidrnetmask(cidr)) && cidr != "0.0.0.0/0"])
    error_message = "Supply explicit operator IPv4 CIDRs."
  }
}
variable "deployment_cidrs" {
  type        = list(string)
  description = "Sources allowed to reach the TLS Kubernetes API (6443); include deployment runner egress."
  validation {
    condition     = length(var.deployment_cidrs) > 0 && alltrue([for cidr in var.deployment_cidrs : can(cidrnetmask(cidr))])
    error_message = "Supply deployment runner IPv4 CIDRs."
  }
}
variable "budget_email" { type = string }
variable "budget_start" {
  type        = string
  description = "First day of current month in RFC3339, e.g. 2026-11-01T00:00:00Z."
}
variable "sas_start" { type = string }
variable "sas_expiry" {
  type        = string
  description = "Explicit SAS expiry; rotate with apply and secret sync before this time. Use <=90 days."
}
variable "vm_size" { default = "Standard_B2ms" }
variable "postgres_sku" { default = "B_Standard_B1ms" }
variable "k3s_version" { default = "v1.37.1+k3s1" }
variable "image_version" {
  type        = string
  description = "Exact Canonical Ubuntu 24.04 image version available in the chosen region; no latest."
  validation {
    condition     = can(regex("^[0-9]+\\.[0-9]+\\.[0-9]+$", var.image_version))
    error_message = "Pin a concrete Ubuntu image version."
  }
}
locals {
  environments = toset(["dev", "staging", "prod"])
  db_tiers     = toset(["nonprod", "prod"])
  tags         = { project = "fishhawk", purpose = "hosted-pilot", issue = "4902" }
}
