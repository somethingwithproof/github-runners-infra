# Secret shells only. Values are added out-of-band (never committed) with:
#   echo -n "<value>" | gcloud secrets versions add <name> --data-file=- \
#     --project=swp-ci-runners
#
# Terraform manages the secret containers and their access policy, not the
# version payloads. The ignore_changes on the resource keeps a manually added
# version from showing as drift.

locals {
  runner_secrets = {
    "github-app-id"          = "GitHub App ID"
    "github-installation-id" = "GitHub App installation ID"
    "github-app-private-key" = "GitHub App private key (PEM)"
    "github-webhook-secret"  = "GitHub webhook HMAC secret"
  }
}

resource "google_secret_manager_secret" "runner" {
  for_each = local.runner_secrets

  project   = google_project.runners.project_id
  secret_id = each.key

  labels = var.labels

  replication {
    auto {}
  }

  depends_on = [google_project_service.enabled]
}
