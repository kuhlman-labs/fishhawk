output "resource_group" { value = data.azurerm_resource_group.pilot.name }
output "public_ip" { value = azurerm_public_ip.app.ip_address }
output "vaults" { value = { for env, vault in azurerm_key_vault.environment : env => vault.name } }
output "deploy_client_ids" { value = { for env, identity in azurerm_user_assigned_identity.deploy : env => identity.client_id } }
output "tenant_id" { value = data.azurerm_client_config.current.tenant_id }
output "database_subnets" { value = { for env, subnet in azurerm_subnet.database : env => subnet.address_prefixes[0] } }
