# SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
# SPDX-License-Identifier: MIT

packer {
  required_version = ">= 1.10"
  required_plugins {
    linode = {
      source  = "github.com/linode/linode"
      version = "~> 1.1"
    }
  }
}

variable "linode_token" {
  type      = string
  default   = env("LINODE_TOKEN")
  sensitive = true
}

variable "runner_version" { type = string }
variable "runner_sha256" { type = string }
variable "region" {
  type    = string
  default = "us-east"
}
variable "instance_type" {
  type    = string
  default = "g6-standard-2"
}

source "linode" "runner" {
  linode_token      = var.linode_token
  image             = "linode/ubuntu24.04"
  region            = var.region
  instance_type     = var.instance_type
  ssh_username      = "root"
  image_label       = "github-runner-${var.runner_version}-{{timestamp}}"
  image_description = "Pre-baked ephemeral GitHub Actions runner (${var.runner_version})"
}

build {
  sources = ["source.linode.runner"]

  provisioner "shell" {
    script = "${path.root}/scripts/install-runner.sh"
    environment_vars = [
      "RUNNER_VERSION=${var.runner_version}",
      "RUNNER_SHA256=${var.runner_sha256}",
    ]
  }
}
