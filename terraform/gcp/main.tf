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

variable "region" {
  description = "GCP region containing the runner subnetwork."
  type        = string
  default     = "us-central1"
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

variable "subnetwork" {
  description = "Existing private subnetwork name for ephemeral runner VMs."
  type        = string
}

variable "controller_service_account_email" {
  description = "Existing service account email used by the webhook controller."
  type        = string
}

variable "controller_role" {
  description = "Optional reviewed project or organization role ID granted to the controller service account."
  type        = string
  default     = ""
}

locals {
  safe_id       = replace(lower(var.controller_id), "_", "-")
  account_name  = "gh-runner-${local.safe_id}"
  preserve_id   = local.safe_id == var.controller_id && !endswith(local.safe_id, "-")
  suffix        = substr(sha256(var.controller_id), 0, 12)
  firewall_name = "gh-runners-${local.safe_id}-deny-inbound"
}

resource "google_service_account" "runner" {
  project      = var.project_id
  account_id   = local.preserve_id && length(local.account_name) <= 30 ? local.account_name : "${substr(local.account_name, 0, 17)}-${local.suffix}"
  display_name = "Ephemeral GitHub Actions Runner (${var.controller_id})"
  description  = "Zero-role identity attached to ephemeral runner VMs"
}

resource "google_service_account_iam_member" "act_as_runner" {
  service_account_id = google_service_account.runner.name
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${var.controller_service_account_email}"
}

resource "google_compute_subnetwork_iam_member" "controller_subnet_use" {
  project    = var.project_id
  region     = var.region
  subnetwork = var.subnetwork
  role       = "roles/compute.networkUser"
  member     = "serviceAccount:${var.controller_service_account_email}"
}

resource "google_project_iam_member" "controller" {
  count   = var.controller_role != "" ? 1 : 0
  project = var.project_id
  role    = var.controller_role
  member  = "serviceAccount:${var.controller_service_account_email}"
}

resource "google_compute_firewall" "deny_inbound" {
  project                 = var.project_id
  name                    = local.preserve_id && length(local.firewall_name) <= 63 ? local.firewall_name : "${substr(local.firewall_name, 0, 50)}-${local.suffix}"
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

output "deny_inbound_firewall_name" {
  description = "Name of the deny-all-inbound firewall rule targeting runner VMs."
  value       = google_compute_firewall.deny_inbound.name
}
