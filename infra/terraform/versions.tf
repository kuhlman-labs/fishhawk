terraform {
  required_version = ">= 1.5.7, < 2.0"
  backend "azurerm" {}
  required_providers {
    azurerm = { source = "hashicorp/azurerm", version = "= 5.12.0" }
    random  = { source = "hashicorp/random", version = "= 3.7.2" }
  }
}
provider "azurerm" {
  features {}
  subscription_id                 = var.subscription_id
  resource_provider_registrations = "none"
}
data "azurerm_client_config" "current" {}
