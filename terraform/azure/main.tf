# SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
# SPDX-License-Identifier: MIT

terraform {
  required_version = ">= 1.6"

  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 4.10"
    }
  }
}

variable "controller_id" {
  description = "Unique controller ownership identifier (CONTROLLER_ID)."
  type        = string

  validation {
    condition     = can(regex("^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$", var.controller_id))
    error_message = "controller_id must start with an alphanumeric character and contain only letters, numbers, underscores, and hyphens (max 64 chars)."
  }
}

variable "location" {
  description = "Azure region for runner VMs (AZURE_LOCATION)."
  type        = string
  default     = "westus2"
}

variable "resource_group_name" {
  description = "Resource group name for ephemeral runner VMs and NICs (AZURE_RESOURCE_GROUP)."
  type        = string
  default     = "github-runners"
}

variable "subnet_id" {
  description = "Existing private subnet ID for runner NICs (AZURE_SUBNET_ID)."
  type        = string
}

variable "tags" {
  description = "Additional tags applied to all created Azure resources."
  type        = map(string)
  default     = {}
}

locals {
  common_tags = merge(var.tags, {
    "github-runners-controller" = var.controller_id
    "ManagedBy"                 = "terraform"
  })
}

resource "azurerm_resource_group" "runners" {
  name     = var.resource_group_name
  location = var.location
  tags     = local.common_tags
}

resource "azurerm_network_security_group" "runners" {
  name                = "nsg-github-runners-${var.controller_id}"
  location            = azurerm_resource_group.runners.location
  resource_group_name = azurerm_resource_group.runners.name
  tags                = local.common_tags

  security_rule {
    name                       = "DenyAllInbound"
    priority                   = 4096
    direction                  = "Inbound"
    access                     = "Deny"
    protocol                   = "*"
    source_port_range          = "*"
    destination_port_range     = "*"
    source_address_prefix      = "*"
    destination_address_prefix = "*"
  }

  security_rule {
    name                       = "AllowHTTPSOutbound"
    priority                   = 100
    direction                  = "Outbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = "443"
    source_address_prefix      = "*"
    destination_address_prefix = "Internet"
  }
}

resource "azurerm_role_definition" "controller_rg" {
  name        = "GitHub Runners Controller RG (${var.controller_id})"
  scope       = azurerm_resource_group.runners.id
  description = "Least-privilege VM, disk, and NIC lifecycle actions for ephemeral runners"

  permissions {
    actions = [
      "Microsoft.Compute/virtualMachines/read",
      "Microsoft.Compute/virtualMachines/write",
      "Microsoft.Compute/virtualMachines/delete",
      "Microsoft.Compute/disks/read",
      "Microsoft.Compute/disks/write",
      "Microsoft.Compute/disks/delete",
      "Microsoft.Network/networkInterfaces/read",
      "Microsoft.Network/networkInterfaces/write",
      "Microsoft.Network/networkInterfaces/delete",
      "Microsoft.Network/networkInterfaces/join/action",
    ]
    not_actions = []
  }

  assignable_scopes = [azurerm_resource_group.runners.id]
}

resource "azurerm_role_definition" "controller_subnet" {
  name        = "GitHub Runners Controller Subnet Join (${var.controller_id})"
  scope       = var.subnet_id
  description = "Least-privilege subnet join action for ephemeral runner NICs"

  permissions {
    actions     = ["Microsoft.Network/virtualNetworks/subnets/join/action"]
    not_actions = []
  }

  assignable_scopes = [var.subnet_id]
}

output "resource_group_name" {
  description = "Value for AZURE_RESOURCE_GROUP."
  value       = azurerm_resource_group.runners.name
}

output "network_security_group_id" {
  description = "NSG ID to associate with the private runner subnet."
  value       = azurerm_network_security_group.runners.id
}

output "controller_rg_role_definition_id" {
  description = "Custom role definition ID scoped to the runner resource group."
  value       = azurerm_role_definition.controller_rg.role_definition_resource_id
}

output "controller_subnet_role_definition_id" {
  description = "Custom role definition ID scoped to the runner subnet."
  value       = azurerm_role_definition.controller_subnet.role_definition_resource_id
}
