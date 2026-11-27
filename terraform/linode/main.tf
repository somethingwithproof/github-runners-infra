# SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
# SPDX-License-Identifier: MIT

terraform {
  required_version = ">= 1.6"

  required_providers {
    linode = {
      source  = "linode/linode"
      version = "~> 2.31"
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
  description = "Linode / Akamai Cloud region ID (LINODE_REGION)."
  type        = string
  default     = "us-east"
}

variable "subnet_ipv4" {
  description = "Private RFC1918 CIDR for the runner VPC subnet."
  type        = string
  default     = "10.90.0.0/24"
}

locals {
  safe_id = replace(lower(var.controller_id), "_", "-")
}

resource "linode_vpc" "runners" {
  label       = substr("ghr-${local.safe_id}", 0, 64)
  region      = var.region
  description = "Dedicated VPC for ephemeral GitHub Actions runners (${var.controller_id})"
}

resource "linode_vpc_subnet" "runners" {
  vpc_id = linode_vpc.runners.id
  label  = "runners"
  ipv4   = var.subnet_ipv4
}

resource "linode_firewall" "runners" {
  label           = substr("ghr-${local.safe_id}-deny-in", 0, 32)
  inbound_policy  = "DROP"
  outbound_policy = "DROP"
  tags            = ["runner-controller-${var.controller_id}"]

  outbound {
    label    = "allow-https"
    action   = "ACCEPT"
    protocol = "TCP"
    ports    = "443"
    ipv4     = ["0.0.0.0/0"]
    ipv6     = ["::/0"]
  }

  outbound {
    label    = "allow-dns-udp"
    action   = "ACCEPT"
    protocol = "UDP"
    ports    = "53"
    ipv4     = ["0.0.0.0/0"]
    ipv6     = ["::/0"]
  }

  outbound {
    label    = "allow-dns-tcp"
    action   = "ACCEPT"
    protocol = "TCP"
    ports    = "53"
    ipv4     = ["0.0.0.0/0"]
    ipv6     = ["::/0"]
  }
}

output "firewall_id" {
  description = "Value for LINODE_FIREWALL_ID."
  value       = linode_firewall.runners.id
}

output "vpc_id" {
  description = "Created Linode VPC ID."
  value       = linode_vpc.runners.id
}

output "vpc_subnet_id" {
  description = "Value for LINODE_VPC_SUBNET_ID."
  value       = linode_vpc_subnet.runners.id
}

output "region" {
  description = "Value for LINODE_REGION."
  value       = var.region
}
