resource "random_password" "admin" {
  for_each = local.db_tiers
  length   = 32
  special  = false
}
resource "random_password" "database" {
  for_each = local.environments
  length   = 32
  special  = false
}
resource "azurerm_postgresql_flexible_server" "database" {
  for_each                      = local.db_tiers
  name                          = "${var.name}-${each.key}"
  resource_group_name           = data.azurerm_resource_group.pilot.name
  location                      = var.location
  version                       = "16"
  administrator_login           = "fishhawkadmin"
  administrator_password        = random_password.admin[each.key].result
  sku_name                      = var.postgres_sku
  storage_mb                    = 32768
  backup_retention_days         = each.key == "prod" ? 14 : 7
  geo_redundant_backup_enabled  = false
  public_network_access_enabled = false
  delegated_subnet_id           = azurerm_subnet.database[each.key].id
  private_dns_zone_id           = azurerm_private_dns_zone.postgres.id
  depends_on                    = [azurerm_private_dns_zone_virtual_network_link.postgres]
  lifecycle { prevent_destroy = true }
  tags = local.tags
}
resource "azurerm_postgresql_flexible_server_database" "environment" {
  for_each  = local.environments
  name      = "fishhawk_${each.key}"
  server_id = azurerm_postgresql_flexible_server.database[each.key == "prod" ? "prod" : "nonprod"].id
  charset   = "UTF8"
  collation = "en_US.utf8"
  lifecycle { prevent_destroy = true }
}
resource "azurerm_storage_account" "traces" {
  for_each                        = local.environments
  name                            = "${var.name}${each.key}"
  resource_group_name             = data.azurerm_resource_group.pilot.name
  location                        = var.location
  account_tier                    = "Standard"
  account_replication_type        = "LRS"
  min_tls_version                 = "TLS1_2"
  allow_nested_items_to_be_public = false
  blob_properties {
    versioning_enabled = true
    delete_retention_policy { days = 14 }
    container_delete_retention_policy { days = 14 }
  }
  lifecycle { prevent_destroy = true }
  tags = merge(local.tags, { environment = each.key })
}
resource "azurerm_storage_container" "traces" {
  for_each              = local.environments
  name                  = "traces"
  storage_account_id    = azurerm_storage_account.traces[each.key].id
  container_access_type = "private"
}
data "azurerm_storage_account_blob_container_sas" "traces" {
  for_each          = local.environments
  connection_string = azurerm_storage_account.traces[each.key].primary_connection_string
  container_name    = azurerm_storage_container.traces[each.key].name
  lifecycle {
    precondition {
      condition     = timecmp(var.sas_expiry, var.sas_start) > 0 && timecmp(var.sas_expiry, timeadd(var.sas_start, "2160h")) <= 0
      error_message = "SAS expiry must follow start and be at most 90 days later."
    }
  }
  https_only = true
  start      = var.sas_start
  expiry     = var.sas_expiry
  permissions {
    read   = true
    add    = false
    create = true
    write  = true
    delete = false
    list   = true
  }
}
