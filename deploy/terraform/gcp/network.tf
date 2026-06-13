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

resource "google_compute_router" "runners" {
  project = google_project.runners.project_id
  name    = "runners-router"
  region  = var.region
  network = google_compute_network.runners.id
}

# NAT gives no-public-IP VMs outbound internet. Subnet-scoped, auto IP
# allocation. No inbound is possible through NAT.
resource "google_compute_router_nat" "runners" {
  project = google_project.runners.project_id
  name    = "runners-nat"
  router  = google_compute_router.runners.name
  region  = var.region

  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "LIST_OF_SUBNETWORKS"

  subnetwork {
    name                    = google_compute_subnetwork.runners.id
    source_ip_ranges_to_nat = ["ALL_IP_RANGES"]
  }

  log_config {
    enable = true
    filter = "ERRORS_ONLY"
  }
}

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
