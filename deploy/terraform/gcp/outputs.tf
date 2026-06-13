output "project_id" {
  description = "Project hosting the runner fleet."
  value       = google_project.runners.project_id
}

output "zone" {
  description = "Default zone for runner and webhook VMs."
  value       = var.zone
}

output "runner_machine_type" {
  description = "Machine type used by the Spot runner template."
  value       = var.runner_machine_type
}

output "runner_node_sa_email" {
  description = "Service account attached to ephemeral runner VMs."
  value       = google_service_account.runner_node.email
}

output "runner_control_sa_email" {
  description = "Service account used by the webhook host to manage runner VMs."
  value       = google_service_account.runner_control.email
}

output "network_self_link" {
  description = "Self link of the runners VPC."
  value       = google_compute_network.runners.self_link
}

output "subnet_self_link" {
  description = "Self link of the runners subnet."
  value       = google_compute_subnetwork.runners.self_link
}

output "secret_ids" {
  description = "Secret Manager secret IDs (values supplied out-of-band)."
  value       = { for k, s in google_secret_manager_secret.runner : k => s.secret_id }
}
