terraform {
  required_version = ">= 1.6"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.10"
    }
  }

  # Remote state. Backend config is partial so it can be overridden per
  # environment with `terraform init -backend-config=...`. For local
  # validation use `terraform init -backend=false`.
  backend "s3" {
    bucket  = "picasso-terraform-state"
    key     = "github-runners/gcp/terraform.tfstate"
    region  = "us-east-1"
    encrypt = true
  }
}

# The project is created by this same configuration (google_project.runners).
# To avoid a provider/resource cycle, the provider's default project is pinned
# to the project_id variable, which matches the created project's id. Resources
# that must wait for the project (and its enabled services) declare explicit
# depends_on instead of relying on the provider block.
provider "google" {
  project = var.project_id
  region  = var.region
}
