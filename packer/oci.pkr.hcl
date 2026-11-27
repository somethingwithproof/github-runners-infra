# SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
# SPDX-License-Identifier: MIT

packer {
  required_version = ">= 1.10"
  required_plugins {
    oracle = {
      source  = "github.com/hashicorp/oracle"
      version = "~> 1.0"
    }
  }
}

variable "runner_version" { type = string }
variable "runner_sha256" { type = string }
variable "availability_domain" { type = string }
variable "compartment_ocid" { type = string }
variable "subnet_ocid" { type = string }
variable "base_image_ocid" { type = string }
variable "shape" {
  type    = string
  default = "VM.Standard.E4.Flex"
}
variable "ocpus" {
  type    = number
  default = 4
}
variable "memory_in_gbs" {
  type    = number
  default = 16
}

source "oracle-oci" "runner" {
  availability_domain = var.availability_domain
  base_image_ocid     = var.base_image_ocid
  compartment_ocid    = var.compartment_ocid
  subnet_ocid         = var.subnet_ocid
  shape               = var.shape
  ssh_username        = "ubuntu"
  image_name          = "github-runner-${var.runner_version}-{{timestamp}}"

  shape_config {
    ocpus         = var.ocpus
    memory_in_gbs = var.memory_in_gbs
  }
}

build {
  sources = ["source.oracle-oci.runner"]

  provisioner "shell" {
    execute_command = "sudo -E bash '{{.Path}}'"
    script          = "${path.root}/scripts/install-runner.sh"
    environment_vars = [
      "RUNNER_VERSION=${var.runner_version}",
      "RUNNER_SHA256=${var.runner_sha256}",
    ]
  }
}
