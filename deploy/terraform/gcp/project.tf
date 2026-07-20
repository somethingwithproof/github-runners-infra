# Dedicated project for the ephemeral runner fleet. Isolating runners in their
# own project keeps the CI blast radius off any production project and makes
# billing and teardown trivial.
resource "google_project" "runners" {
  name            = var.project_name
  project_id      = var.project_id
  billing_account = var.billing_account

  # org_id and folder_id are mutually exclusive. Pass exactly one; both empty
  # means the project is created under the active gcloud config's parent.
  org_id    = var.org_id != "" ? var.org_id : null
  folder_id = var.folder_id != "" ? var.folder_id : null

  labels = var.labels

  # Deleting the default network is handled by not enabling the
  # compute.googleapis.com default-network behavior; we create our own VPC.
  auto_create_network = false
}

locals {
  required_services = [
    "compute.googleapis.com",
    "iam.googleapis.com",
    "secretmanager.googleapis.com",
    "logging.googleapis.com",
  ]
}

resource "google_project_service" "enabled" {
  for_each = toset(local.required_services)

  project = google_project.runners.project_id
  service = each.value

  # Keep APIs enabled if Terraform is destroyed; disabling a service can cascade
  # delete resources outside this state.
  disable_on_destroy = false
}
