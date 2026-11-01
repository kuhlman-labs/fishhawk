resource "azurerm_key_vault" "environment" {
  for_each                   = local.environments
  name                       = "${var.name}-${each.key}"
  location                   = var.location
  resource_group_name        = data.azurerm_resource_group.pilot.name
  tenant_id                  = data.azurerm_client_config.current.tenant_id
  sku_name                   = "standard"
  rbac_authorization_enabled = true
  purge_protection_enabled   = true
  soft_delete_retention_days = 14
  tags                       = merge(local.tags, { environment = each.key })
}
resource "azurerm_role_assignment" "terraform_secrets" {
  for_each             = local.environments
  scope                = azurerm_key_vault.environment[each.key].id
  role_definition_name = "Key Vault Secrets Officer"
  principal_id         = data.azurerm_client_config.current.object_id
}
resource "azurerm_role_assignment" "node_secrets" {
  for_each             = local.environments
  scope                = azurerm_key_vault.environment[each.key].id
  role_definition_name = "Key Vault Secrets User"
  principal_id         = azurerm_linux_virtual_machine.app.identity[0].principal_id
}
resource "azurerm_user_assigned_identity" "deploy" {
  for_each            = local.environments
  name                = "${var.name}-deploy-${each.key}"
  location            = var.location
  resource_group_name = data.azurerm_resource_group.pilot.name
}
resource "azurerm_federated_identity_credential" "deploy" {
  for_each                  = local.environments
  name                      = "github-${each.key}"
  user_assigned_identity_id = azurerm_user_assigned_identity.deploy[each.key].id
  audience                  = ["api://AzureADTokenExchange"]
  issuer                    = "https://token.actions.githubusercontent.com"
  subject                   = "repo:${var.github_repository}:environment:${each.key}"
}
# Only the environment's Kubernetes deployment credential is readable by CI.
# Application and DB secrets are synced by the operator, not read by deploy jobs.
resource "azurerm_role_assignment" "deployment_credential" {
  for_each             = local.environments
  scope                = azurerm_key_vault_secret.deploy[each.key].resource_versionless_id
  role_definition_name = "Key Vault Secrets User"
  principal_id         = azurerm_user_assigned_identity.deploy[each.key].principal_id
}
locals {
  generated_secrets = merge([
    for env in local.environments : {
      "${env}/database-url" = {
        env   = env
        value = "postgres://fishhawk_${env}:${random_password.database[env].result}@${azurerm_postgresql_flexible_server.database[env == "prod" ? "prod" : "nonprod"].fqdn}:5432/fishhawk_${env}?sslmode=verify-full"
      }
      "${env}/database-password" = { env = env, value = random_password.database[env].result }
      "${env}/database-admin-url" = {
        env   = env
        value = "postgres://fishhawkadmin:${random_password.admin[env == "prod" ? "prod" : "nonprod"].result}@${azurerm_postgresql_flexible_server.database[env == "prod" ? "prod" : "nonprod"].fqdn}:5432/postgres?sslmode=verify-full"
      }
      "${env}/azure-storage-connection-string" = {
        env   = env
        value = "BlobEndpoint=${azurerm_storage_account.traces[env].primary_blob_endpoint};SharedAccessSignature=${trimprefix(data.azurerm_storage_account_blob_container_sas.traces[env].sas, "?")}"
      }
    }
  ]...)
}
resource "azurerm_key_vault_secret" "generated" {
  for_each     = toset(flatten([for env in local.environments : [for name in ["database-url", "database-password", "database-admin-url", "azure-storage-connection-string"] : "${env}/${name}"]]))
  name         = split("/", each.key)[1]
  value        = local.generated_secrets[each.key].value
  key_vault_id = azurerm_key_vault.environment[local.generated_secrets[each.key].env].id
  depends_on   = [azurerm_role_assignment.terraform_secrets]
}

resource "azurerm_key_vault_secret" "deploy" {
  for_each     = local.environments
  name         = "deploy-kubeconfig"
  value        = "NOT_BOOTSTRAPPED"
  key_vault_id = azurerm_key_vault.environment[each.key].id
  depends_on   = [azurerm_role_assignment.terraform_secrets]
  lifecycle { ignore_changes = [value] }
}
