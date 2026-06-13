variable "project_id" {
  type        = string
  description = "ID of the dedicated GCP project that hosts the runner fleet."
  default     = "swp-ci-runners"
}

variable "project_name" {
  type        = string
  description = "Human-readable display name for the project."
  default     = "SWP CI Runners"
}

variable "billing_account" {
  type        = string
  description = "Billing account ID to associate with the project."
  default     = "01B4AB-92453F-05CB01"
}

variable "org_id" {
  type        = string
  description = "Organization ID to create the project under. Mutually exclusive with folder_id; leave empty if using folder_id."
  default     = ""
}

variable "folder_id" {
  type        = string
  description = "Folder ID to create the project under. Mutually exclusive with org_id; leave empty if using org_id."
  default     = ""
}

variable "region" {
  type        = string
  description = "Region for the network, subnet, NAT, and instances."
  default     = "us-central1"
}

variable "zone" {
  type        = string
  description = "Zone for the webhook-host VM and the runner template default."
  default     = "us-central1-a"
}

variable "subnet_cidr" {
  type        = string
  description = "Primary IPv4 range for the runners subnet."
  default     = "10.20.0.0/24"
}

variable "runner_machine_type" {
  type        = string
  description = "Machine type for ephemeral Spot runner VMs. Default matches the prior DO s-4vcpu-8gb (4 vCPU / 8 GB)."
  default     = "e2-custom-4-8192"
}

variable "webhook_machine_type" {
  type        = string
  description = "Machine type for the long-lived webhook host. e2-micro is free-tier eligible in us-central1."
  default     = "e2-micro"
}

variable "runner_image" {
  type        = string
  description = "Boot image for runner and webhook VMs."
  default     = "projects/debian-cloud/global/images/family/debian-12"
}

variable "runner_boot_disk_gb" {
  type        = number
  description = "Boot disk size in GB for runner VMs."
  default     = 50
}

variable "runner_startup_script" {
  type        = string
  description = <<-EOT
    Startup script (cloud-init style) for ephemeral runners. The default is a
    placeholder. The real script installs Docker and the GitHub Actions runner,
    registers it with --ephemeral, runs a single job, then deletes its own VM.
    Supply the production script via tfvars or wire it to a cloud-init file.
  EOT
  default     = <<-EOT
    #!/usr/bin/env bash
    set -euo pipefail
    # PLACEHOLDER. Replace with the real bootstrap:
    #   1. install docker + the actions runner
    #   2. fetch a short-lived registration token (webhook host mints it)
    #   3. ./config.sh --ephemeral --url <repo/org> --token <token>
    #   4. ./run.sh           # processes exactly one job then exits
    #   5. gcloud compute instances delete "$(hostname)" --zone <zone> -q
    echo "runner startup placeholder"
  EOT
}

variable "labels" {
  type        = map(string)
  description = "Labels applied to project and compute resources."
  default = {
    system     = "github-runners"
    managed_by = "terraform"
  }
}
