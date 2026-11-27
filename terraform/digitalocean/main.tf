# SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
# SPDX-License-Identifier: MIT

terraform {
  required_version = ">= 1.6"

  required_providers {
    digitalocean = {
      source  = "digitalocean/digitalocean"
      version = "~> 2.40"
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
  description = "DigitalOcean region slug for runner droplets."
  type        = string
  default     = "nyc3"
}

variable "vpc_ip_range" {
  description = "Private RFC1918 CIDR block for the dedicated runner VPC."
  type        = string
  default     = "10.88.0.0/20"
}

locals {
  controller_tag = "runner-controller-${var.controller_id}"
}

resource "digitalocean_tag" "controller" {
  name = local.controller_tag
}

resource "digitalocean_vpc" "runners" {
  name        = "github-runners-${var.controller_id}"
  region      = var.region
  ip_range    = var.vpc_ip_range
  description = "Dedicated private VPC for ephemeral GitHub Actions runners (${var.controller_id})"
}

resource "digitalocean_firewall" "runners" {
  name = "github-runners-${var.controller_id}-deny-inbound"
  tags = [digitalocean_tag.controller.name]

  outbound_rule {
    protocol              = "tcp"
    port_range            = "443"
    destination_addresses = ["0.0.0.0/0", "::/0"]
  }

  outbound_rule {
    protocol              = "udp"
    port_range            = "53"
    destination_addresses = ["0.0.0.0/0", "::/0"]
  }

  outbound_rule {
    protocol              = "tcp"
    port_range            = "53"
    destination_addresses = ["0.0.0.0/0", "::/0"]
  }
}

output "vpc_uuid" {
  description = "Value for DO_VPC_UUID."
  value       = digitalocean_vpc.runners.id
}

output "firewall_id" {
  description = "Value for DO_FIREWALL_ID."
  value       = digitalocean_firewall.runners.id
}

output "controller_tag" {
  description = "Ownership tag targeted by the deny-inbound firewall."
  value       = digitalocean_tag.controller.name
}

output "region" {
  description = "Value for DO_REGION."
  value       = var.region
}
