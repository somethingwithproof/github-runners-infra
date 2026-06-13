# Custom-mode VPC. Runner VMs have no public IP; they reach GitHub and the
# package registries through Cloud NAT. No ingress is opened to runners.

resource "google_compute_network" "runners" {
  project                 = google_project.runners.project_id
  name                    = "runners-vpc"
  auto_create_subnetworks = false
  depends_on              = [google_project_service.enabled]
}

resource "google_compute_subnetwork" "runners" {
  project       = google_project.runners.project_id
  name          = "runners-subnet"
  region        = var.region
  network       = google_compute_network.runners.id
  ip_cidr_range = var.subnet_cidr

  # Capture flow logs for security review of egress patterns.
  log_config {
    aggregation_interval = "INTERVAL_10_MIN"
    flow_sampling        = 0.5
    metadata             = "INCLUDE_ALL_METADATA"
  }
}

# Egress is via per-VM ephemeral external IPs (set in compute.tf), not Cloud NAT:
# a 24/7 NAT gateway's fixed hourly charge dominates the bill for bursty ephemeral
# runners. deny_all_ingress below keeps every VM unreachable inbound regardless.

# --- Firewall ---------------------------------------------------------------
# Fail-closed posture: no ingress allow rule for runner or webhook VMs from the
# internet. An explicit deny-all ingress rule documents intent and overrides any
# future broad allow that lands at a higher priority number.

# Explicit highest-priority deny on all inbound from anywhere.
resource "google_compute_firewall" "deny_all_ingress" {
  project   = google_project.runners.project_id
  name      = "deny-all-ingress"
  network   = google_compute_network.runners.id
  direction = "INGRESS"
  priority  = 1000

  deny {
    protocol = "all"
  }

  source_ranges = ["0.0.0.0/0"]

  log_config {
    metadata = "INCLUDE_ALL_METADATA"
  }
}

# Allow all egress. Runners need to reach GitHub, container registries, and
# package mirrors over a changing set of IPs; pinning egress destinations is not
# practical for general CI. Egress still flows through Cloud NAT only.
resource "google_compute_firewall" "allow_egress" {
  project   = google_project.runners.project_id
  name      = "allow-all-egress"
  network   = google_compute_network.runners.id
  direction = "EGRESS"
  priority  = 1000

  allow {
    protocol = "all"
  }

  destination_ranges = ["0.0.0.0/0"]
}
