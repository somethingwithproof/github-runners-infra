# SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
# SPDX-License-Identifier: MIT

packer {
  required_version = ">= 1.10"
  required_plugins {
    vultr = {
      source  = "github.com/vultr/vultr"
      version = "~> 2.7"
    }
  }
}

variable "runner_version" { type = string }
variable "runner_sha256" { type = string }
variable "region" {
  type    = string
  default = "ewr"
}
variable "plan" {
  type    = string
  default = "vc2-2c-4gb"
}

source "vultr" "runner" {
  api_key              = env("VULTR_API_KEY")
  os_id                = 2284
  region_id            = var.region
  plan_id              = var.plan
  ssh_username         = "root"
  snapshot_description = "github-runner-${var.runner_version}-{{timestamp}}"
}

build {
  sources = ["source.vultr.runner"]

  provisioner "shell" {
    script = "${path.root}/scripts/install-runner.sh"
    environment_vars = [
      "RUNNER_VERSION=${var.runner_version}",
      "RUNNER_SHA256=${var.runner_sha256}",
    ]
  }
}
