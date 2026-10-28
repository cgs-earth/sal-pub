terraform {
  required_version = ">= 1.6" # Terraform or OpenTofu

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.35"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }

  # State holds generated secrets, so keep it in a private bucket:
  #   tofu init -backend-config="bucket=terraform-state" -backend-config="prefix=sal-pub"
  backend "gcs" {
    bucket = "sal--terraform-state"
    prefix = "sal-pub"
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}

data "google_client_config" "current" {}

# Talks to the cluster created in gke.tf, so the Kubernetes resources in
# k8s.tf are applied in the same run.
provider "kubernetes" {
  host                   = "https://${google_container_cluster.this.endpoint}"
  token                  = data.google_client_config.current.access_token
  cluster_ca_certificate = base64decode(google_container_cluster.this.master_auth[0].cluster_ca_certificate)
}
