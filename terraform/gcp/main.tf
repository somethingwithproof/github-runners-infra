# SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
# SPDX-License-Identifier: MIT

terraform {
  required_version = ">= 1.6"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.10"
    }
  }
}

variable "project_id" {
  description = "GCP project ID (GCP_PROJECT_ID)."
  type        = string
}

variable "controller_id" {
  description = "Unique controller ownership identifier (CONTROLLER_ID)."
  type        = string

  validation {
    condition     = can(regex("^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$", var.controller_id))
    error_message = "controller_id must start with an alphanumeric character and contain only letters, numbers, underscores, and hyphens (max 64 chars)."
  }
}

variable "network" {
  description = "VPC network name or self_link for runner firewall rules."
  type        = string
}

variable "controller_service_account_email" {
  description = "Existing service account email used by the webhook controller."
  type        = string
}

locals {
  safe_id = replace(lower(var.controller_id), "_", "-")
}

resource "google_service_account" "runner" {
  project      = var.project_id
  account_id   = substr("gh-runner-${local.safe_id}", 0, 30)
  display_name = "Ephemeral GitHub Actions Runner (${var.controller_id})"
  description  = "Zero-role identity attached to ephemeral runner VMs"
}

resource "google_project_iam_custom_role" "controller" {
  project     = var.project_id
  role_id     = replace("github_runners_controller_${lower(var.controller_id)}", "-", "_")
  title       = "GitHub Runners Controller (${var.controller_id})"
  description = "Least-privilege Compute Engine permissions for ephemeral runner lifecycle"
  permissions = [
    "compute.instances.create",
    "compute.instances.get",
    "compute.instances.list",
    "compute.instances.delete",
    "compute.zoneOperations.get",
    "compute.images.useReadOnly",
    "compute.disks.create",
    "compute.subnetworks.use",
    "compute.instances.setLabels",
    "compute.instances.setMetadata",
    "compute.instances.setServiceAccount",
  ]
}

resource "google_project_iam_member" "controller" {
  project = var.project_id
  role    = google_project_iam_custom_role.controller.name
  member  = "serviceAccount:${var.controller_service_account_email}"
}

resource "google_service_account_iam_member" "act_as_runner" {
  service_account_id = google_service_account.runner.name
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${var.controller_service_account_email}"
}

resource "google_compute_firewall" "deny_inbound" {
  project                 = var.project_id
  name                    = "gh-runners-${local.safe_id}-deny-inbound"
  network                 = var.network
  direction               = "INGRESS"
  priority                = 1000
  target_service_accounts = [google_service_account.runner.email]
  source_ranges           = ["0.0.0.0/0"]

  deny {
    protocol = "all"
  }
}

output "runner_service_account_email" {
  description = "Value for GCP_RUNNER_SERVICE_ACCOUNT_EMAIL."
  value       = google_service_account.runner.email
}

output "controller_custom_role_id" {
  description = "Least-privilege custom role granted to the controller service account."
  value       = google_project_iam_custom_role.controller.id
}
