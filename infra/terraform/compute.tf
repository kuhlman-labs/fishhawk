resource "azurerm_linux_virtual_machine" "app" {
  name                            = "${var.name}-app"
  location                        = var.location
  resource_group_name             = data.azurerm_resource_group.pilot.name
  size                            = var.vm_size
  admin_username                  = "fishhawk"
  network_interface_ids           = [azurerm_network_interface.app.id]
  disable_password_authentication = true
  patch_mode                      = "AutomaticByPlatform"
  provision_vm_agent              = true
  identity { type = "SystemAssigned" }
  admin_ssh_key {
    username   = "fishhawk"
    public_key = var.ssh_public_key
  }
  os_disk {
    caching              = "ReadWrite"
    storage_account_type = "StandardSSD_LRS"
    disk_size_gb         = 32
  }
  source_image_reference {
    publisher = "Canonical"
    offer     = "ubuntu-24_04-lts"
    sku       = "server"
    version   = var.image_version
  }
  custom_data = base64encode(templatefile("${path.module}/templates/cloud-init.yaml.tftpl", {
    k3s_version = var.k3s_version
    public_ip   = azurerm_public_ip.app.ip_address
  }))
  tags = local.tags
}
resource "azurerm_consumption_budget_resource_group" "pilot" {
  name              = "${var.name}-monthly"
  resource_group_id = data.azurerm_resource_group.pilot.id
  amount            = 120
  time_grain        = "Monthly"
  time_period { start_date = var.budget_start }
  dynamic "notification" {
    for_each = [50, 80, 100]
    content {
      enabled        = true
      threshold      = notification.value
      operator       = "GreaterThanOrEqualTo"
      contact_emails = [var.budget_email]
    }
  }
}
