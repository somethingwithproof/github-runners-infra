# SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
# SPDX-License-Identifier: MIT

packer {
  required_version = ">= 1.10"
  required_plugins {
    digitalocean = {
      source  = "github.com/digitalocean/digitalocean"
      version = "~> 1.4"
    }
  }
}

variable "digitalocean_token" {
  type      = string
  default   = env("DIGITALOCEAN_TOKEN")
  sensitive = true
}

variable "runner_version" { type = string }
variable "runner_sha256" { type = string }
variable "region" {
  type    = string
  default = "nyc3"
}
variable "size" {
  type    = string
  default = "s-2vcpu-4gb-amd"
}

source "digitalocean" "runner" {
  api_token     = var.digitalocean_token
  image         = "ubuntu-24-04-x64"
  region        = var.region
  size          = var.size
  ssh_username  = "root"
  snapshot_name = "github-runner-${var.runner_version}-{{timestamp}}"
}

build {
  sources = ["source.digitalocean.runner"]

  provisioner "shell" {
    script = "${path.root}/scripts/install-runner.sh"
    environment_vars = [
      "RUNNER_VERSION=${var.runner_version}",
      "RUNNER_SHA256=${var.runner_sha256}",
    ]
  }
}
