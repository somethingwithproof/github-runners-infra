# SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
# SPDX-License-Identifier: MIT

packer {
  required_version = ">= 1.10"
  required_plugins {
    azure = {
      source  = "github.com/hashicorp/azure"
      version = "~> 2.2"
    }
  }
}

variable "runner_version" { type = string }
variable "runner_sha256" { type = string }
variable "resource_group" { type = string }
variable "location" {
  type    = string
  default = "westus2"
}

source "azure-arm" "runner" {
  subscription_id                   = env("ARM_SUBSCRIPTION_ID")
  use_azure_cli_auth                = true
  os_type                           = "Linux"
  image_publisher                   = "Canonical"
  image_offer                       = "ubuntu-24_04-lts"
  image_sku                         = "server"
  vm_size                           = "Standard_D2s_v5"
  location                          = var.location
  managed_image_resource_group_name = var.resource_group
  managed_image_name                = "github-runner-${var.runner_version}-{{timestamp}}"
}

build {
  sources = ["source.azure-arm.runner"]

  provisioner "shell" {
    execute_command = "sudo -E bash '{{.Path}}'"
    script          = "${path.root}/scripts/install-runner.sh"
    environment_vars = [
      "RUNNER_VERSION=${var.runner_version}",
      "RUNNER_SHA256=${var.runner_sha256}",
    ]
  }
}
