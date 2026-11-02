# One-time operator-owned bootstrap. Keep its local state securely backed up.
terraform {
  required_version = ">= 1.5.7, < 2.0"
  required_providers {
    azurerm = { source = "hashicorp/azurerm", version = "= 5.12.0" }
  }
}
provider "azurerm" {
  features {}
  subscription_id                 = var.subscription_id
  resource_provider_registrations = "none"
  storage_use_azuread             = true
}
variable "subscription_id" { type = string }
variable "name" { type = string }
variable "location" { default = "eastus" }
variable "github_repository" { default = "kuhlman-labs/fishhawk" }
data "azurerm_client_config" "current" {}
resource "azurerm_resource_group" "state" {
  name     = "${var.name}-state"
  location = var.location
}
resource "azurerm_resource_group" "pilot" {
  lifecycle { prevent_destroy = true }
  name     = "${var.name}-pilot"
  location = var.location
  tags     = { project = "fishhawk", purpose = "hosted-pilot", issue = "4902" }
}
resource "azurerm_storage_account" "state" {
  name                            = "${var.name}tfstate"
  location                        = var.location
  resource_group_name             = azurerm_resource_group.state.name
  account_tier                    = "Standard"
  account_replication_type        = "LRS"
  min_tls_version                 = "TLS1_2"
  allow_nested_items_to_be_public = false
  shared_access_key_enabled       = false
  blob_properties {
    versioning_enabled = true
    delete_retention_policy { days = 30 }
  }
  lifecycle { prevent_destroy = true }
}
# ARM container creation does not require account keys.
resource "azurerm_storage_container" "state" {
  depends_on            = [azurerm_role_assignment.state]
  name                  = "tfstate"
  storage_account_id    = azurerm_storage_account.state.id
  container_access_type = "private"
}
resource "azurerm_user_assigned_identity" "infra" {
  name                = "${var.name}-infra"
  location            = var.location
  resource_group_name = azurerm_resource_group.state.name
}
resource "azurerm_federated_identity_credential" "infra" {
  name                      = "github-infra"
  user_assigned_identity_id = azurerm_user_assigned_identity.infra.id
  audience                  = ["api://AzureADTokenExchange"]
  issuer                    = "https://token.actions.githubusercontent.com"
  subject                   = "repo:${var.github_repository}:environment:infra"
}
resource "azurerm_role_assignment" "infra" {
  for_each             = toset(["Contributor", "Role Based Access Control Administrator", "Key Vault Secrets Officer"])
  scope                = azurerm_resource_group.pilot.id
  role_definition_name = each.key
  principal_id         = azurerm_user_assigned_identity.infra.principal_id
}
resource "azurerm_role_assignment" "state" {
  for_each             = { operator = data.azurerm_client_config.current.object_id, infra = azurerm_user_assigned_identity.infra.principal_id }
  scope                = azurerm_storage_account.state.id
  role_definition_name = "Storage Blob Data Contributor"
  principal_id         = each.value
}
output "state_account" { value = azurerm_storage_account.state.name }
output "infra_client_id" { value = azurerm_user_assigned_identity.infra.client_id }
output "tenant_id" { value = data.azurerm_client_config.current.tenant_id }
