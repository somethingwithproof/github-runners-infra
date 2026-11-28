# SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
# SPDX-License-Identifier: MIT

terraform {
  required_version = ">= 1.6"

  required_providers {
    vultr = {
      source  = "vultr/vultr"
      version = "~> 2.23"
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

variable "region" {
  description = "Vultr region code (VULTR_REGION)."
  type        = string
  default     = "ewr"
}

variable "vpc_subnet" {
  description = "Private RFC1918 subnet base address for the dedicated runner VPC."
  type        = string
  default     = "10.89.0.0"
}

variable "vpc_subnet_mask" {
  description = "Prefix length for the runner VPC subnet."
  type        = number
  default     = 20

  validation {
    condition     = var.vpc_subnet_mask >= 16 && var.vpc_subnet_mask <= 28
    error_message = "vpc_subnet_mask must be between 16 and 28."
  }
}

resource "vultr_vpc" "runners" {
  description    = "github-runners-${var.controller_id}"
  region         = var.region
  v4_subnet      = var.vpc_subnet
  v4_subnet_mask = var.vpc_subnet_mask
}

# Vultr starts filtering once a group has a rule. Loopback sources admit no remote peers.
resource "vultr_firewall_group" "runners" {
  description = "github-runners-${var.controller_id}-deny-inbound"
}

resource "vultr_firewall_rule" "loopback" {
  for_each = {
    v4 = { address = "127.0.0.1", bits = 32 }
    v6 = { address = "::1", bits = 128 }
  }
  firewall_group_id = vultr_firewall_group.runners.id
  ip_type           = each.key
  protocol          = "tcp"
  port              = "1"
  subnet            = each.value.address
  subnet_size       = each.value.bits
}

output "vpc_id" {
  description = "Value for VULTR_VPC_ID."
  value       = vultr_vpc.runners.id
}

output "firewall_group_id" {
  depends_on  = [vultr_firewall_rule.loopback]
  description = "Value for VULTR_FIREWALL_GROUP_ID."
  value       = vultr_firewall_group.runners.id
}

output "region" {
  description = "Value for VULTR_REGION."
  value       = var.region
}
