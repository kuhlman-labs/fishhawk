data "azurerm_resource_group" "pilot" {
  name = "${var.name}-pilot"
}
resource "azurerm_virtual_network" "pilot" {
  name                = var.name
  location            = var.location
  resource_group_name = data.azurerm_resource_group.pilot.name
  address_space       = ["10.42.0.0/16"]
}
resource "azurerm_subnet" "app" {
  name                 = "application"
  resource_group_name  = data.azurerm_resource_group.pilot.name
  virtual_network_name = azurerm_virtual_network.pilot.name
  address_prefixes     = ["10.42.1.0/24"]
}
resource "azurerm_subnet" "database" {
  for_each             = local.db_tiers
  name                 = "postgres-${each.key}"
  resource_group_name  = data.azurerm_resource_group.pilot.name
  virtual_network_name = azurerm_virtual_network.pilot.name
  address_prefixes     = [each.key == "prod" ? "10.42.3.0/24" : "10.42.2.0/24"]
  delegation {
    name = "postgres"
    service_delegation {
      name    = "Microsoft.DBforPostgreSQL/flexibleServers"
      actions = ["Microsoft.Network/virtualNetworks/subnets/join/action"]
    }
  }
}
resource "azurerm_private_dns_zone" "postgres" {
  name                = "${var.name}.postgres.database.azure.com"
  resource_group_name = data.azurerm_resource_group.pilot.name
}
resource "azurerm_private_dns_zone_virtual_network_link" "postgres" {
  name                = var.name
  private_dns_zone_id = azurerm_private_dns_zone.postgres.id
  virtual_network_id  = azurerm_virtual_network.pilot.id
}
resource "azurerm_public_ip" "app" {
  name                = "${var.name}-app"
  resource_group_name = data.azurerm_resource_group.pilot.name
  location            = var.location
  allocation_method   = "Static"
  sku                 = "Standard"
}
resource "azurerm_network_security_group" "app" {
  name                = "${var.name}-app"
  resource_group_name = data.azurerm_resource_group.pilot.name
  location            = var.location
  security_rule {
    name                       = "https"
    priority                   = 100
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_ranges    = ["80", "443"]
    source_address_prefix      = "Internet"
    destination_address_prefix = "*"
  }
  security_rule {
    name                       = "operator-ssh"
    priority                   = 110
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = "22"
    source_address_prefixes    = var.admin_cidrs
    destination_address_prefix = "*"
  }
  security_rule {
    name                       = "deployment-api"
    priority                   = 120
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = "6443"
    source_address_prefixes    = var.deployment_cidrs
    destination_address_prefix = "*"
  }
}
resource "azurerm_network_interface" "app" {
  name                = "${var.name}-app"
  location            = var.location
  resource_group_name = data.azurerm_resource_group.pilot.name
  ip_configuration {
    name                          = "app"
    subnet_id                     = azurerm_subnet.app.id
    private_ip_address_allocation = "Dynamic"
    public_ip_address_id          = azurerm_public_ip.app.id
  }
}
resource "azurerm_network_interface_security_group_association" "app" {
  network_interface_id      = azurerm_network_interface.app.id
  network_security_group_id = azurerm_network_security_group.app.id
}
