# --- Ephemeral Spot runner instance template --------------------------------
# The control plane (runner-control SA on the webhook host) creates VMs from
# this template, one per job. Spot pricing because a preempted CI job just
# re-queues. Ephemeral external IPs provide outbound access; ingress is denied.
resource "google_compute_instance_template" "runner" {
  project     = google_project.runners.project_id
  name_prefix = "runner-"
  region      = var.region

  machine_type = var.runner_machine_type

  labels = var.labels
  tags   = ["github-runner"]

  scheduling {
    provisioning_model = "SPOT"
    preemptible        = true
    automatic_restart  = false
    # Spot/preemptible instances must terminate (not migrate) on maintenance.
    on_host_maintenance = "TERMINATE"
  }

  disk {
    source_image = var.runner_image
    auto_delete  = true
    boot         = true
    disk_size_gb = var.runner_boot_disk_gb
  }

  network_interface {
    subnetwork = google_compute_subnetwork.runners.id
    # Ephemeral external IP for egress only. Cheaper than a 24/7 Cloud NAT
    # gateway for bursty ephemeral runners; deny_all_ingress keeps the VM
    # unreachable inbound, so the public IP is outbound-only.
    access_config {}
  }

  service_account {
    email = google_service_account.runner_node.email
    # Least privilege: the runner only writes logs. runner_node has just
    # logging.logWriter in IAM; aligning the OAuth scope keeps a compromised CI
    # job from minting broad-scoped tokens.
    scopes = ["https://www.googleapis.com/auth/logging.write"]
  }

  metadata = {
    startup-script         = var.runner_startup_script
    serial-port-enable     = "FALSE"
    block-project-ssh-keys = "TRUE"
  }

  lifecycle {
    create_before_destroy = true
  }

  depends_on = [google_project_service.enabled]
}

# --- Webhook host -----------------------------------------------------------
# Long-lived e2-micro (free-tier eligible in us-central1). Runs the webhook
# receiver and the control loop that mints registration tokens and spins runner
# VMs up/down. Carries the runner-control SA.
#
# No inbound firewall rule. Its ephemeral public IP is egress-only; GitHub webhook delivery arrives
# through a cloudflared tunnel that is provisioned separately (outbound
# connection from this host to Cloudflare's edge; nothing is opened inbound).
resource "google_compute_instance" "webhook_host" {
  project      = google_project.runners.project_id
  name         = "runner-webhook-host"
  zone         = var.zone
  machine_type = var.webhook_machine_type

  labels = var.labels
  tags   = ["webhook-host"]

  boot_disk {
    initialize_params {
      image = var.runner_image
    }
  }

  network_interface {
    subnetwork = google_compute_subnetwork.runners.id
    # Ephemeral external IP for egress (reach GitHub + GCP APIs and open the
    # cloudflared tunnel outbound). deny_all_ingress keeps it unreachable inbound.
    access_config {}
  }

  service_account {
    email  = google_service_account.runner_control.email
    scopes = ["cloud-platform"]
  }

  shielded_instance_config {
    enable_secure_boot          = true
    enable_vtpm                 = true
    enable_integrity_monitoring = true
  }

  depends_on = [google_project_service.enabled]
}
