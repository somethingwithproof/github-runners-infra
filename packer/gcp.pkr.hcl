# SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
# SPDX-License-Identifier: MIT

packer {
  required_version = ">= 1.10"
  required_plugins {
    googlecompute = {
      source  = "github.com/hashicorp/googlecompute"
      version = "~> 1.1"
    }
  }
}

variable "runner_version" { type = string }
variable "runner_sha256" { type = string }
variable "project_id" { type = string }
variable "zone" {
  type    = string
  default = "us-central1-a"
}

source "googlecompute" "runner" {
  project_id          = var.project_id
  zone                = var.zone
  machine_type        = "e2-standard-4"
  source_image_family = "ubuntu-2404-lts-amd64"
  ssh_username        = "ubuntu"
  image_name          = "github-runner-${replace(var.runner_version, ".", "-")}-{{timestamp}}"
  image_family        = "github-runners"
}

build {
  sources = ["source.googlecompute.runner"]

  provisioner "shell" {
    execute_command = "sudo -E bash '{{.Path}}'"
    script          = "${path.root}/scripts/install-runner.sh"
    environment_vars = [
      "RUNNER_VERSION=${var.runner_version}",
      "RUNNER_SHA256=${var.runner_sha256}",
    ]
  }
}
