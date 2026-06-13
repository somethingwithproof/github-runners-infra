# Two service accounts, each scoped to exactly what it needs.
#
# runner-control: runs on the webhook host. Creates and deletes runner VMs in
# response to GitHub job events. Needs to manage instances, act as the
# runner-node SA (to attach it to VMs it creates), and read the four secrets.
#
# runner-node: attached to ephemeral runner VMs. The runner job runs under this
# identity, so it gets the absolute minimum: write logs. Nothing else.

resource "google_service_account" "runner_control" {
  project      = google_project.runners.project_id
  account_id   = "runner-control"
  display_name = "Runner control plane (webhook host)"
  depends_on   = [google_project_service.enabled]
}

resource "google_service_account" "runner_node" {
  project      = google_project.runners.project_id
  account_id   = "runner-node"
  display_name = "Ephemeral runner VM identity"
  depends_on   = [google_project_service.enabled]
}

# --- runner-control project-level grants ------------------------------------

# Create/start/stop/delete runner VMs. instanceAdmin.v1 is the narrowest
# predefined role that covers the full instance lifecycle without granting
# network or IAM admin.
resource "google_project_iam_member" "control_instance_admin" {
  project = google_project.runners.project_id
  role    = "roles/compute.instanceAdmin.v1"
  member  = "serviceAccount:${google_service_account.runner_control.email}"
}

# --- runner-control scoped (resource-level) grants --------------------------

# Allow the control SA to attach ONLY the runner-node SA to the VMs it creates.
# Granted on the runner-node SA resource, not project-wide, so it cannot
# impersonate or attach any other service account.
resource "google_service_account_iam_member" "control_uses_node_sa" {
  service_account_id = google_service_account.runner_node.name
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${google_service_account.runner_control.email}"
}

# Read access to the four secrets, granted per-secret (not project-wide).
resource "google_secret_manager_secret_iam_member" "control_secret_accessor" {
  for_each = google_secret_manager_secret.runner

  project   = google_project.runners.project_id
  secret_id = each.value.secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.runner_control.email}"
}

# --- runner-node grants -----------------------------------------------------

# The only thing a runner job's identity may do at the cloud level: emit logs.
resource "google_project_iam_member" "node_log_writer" {
  project = google_project.runners.project_id
  role    = "roles/logging.logWriter"
  member  = "serviceAccount:${google_service_account.runner_node.email}"
}
