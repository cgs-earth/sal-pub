# Least-privilege identity for Autopilot nodes (instead of the default compute
# service account): logging, monitoring and pulling from Artifact Registry.
resource "google_service_account" "nodes" {
  account_id   = "${var.name}-nodes"
  display_name = "sal-pub GKE nodes"
}

resource "google_project_iam_member" "nodes" {
  for_each = toset([
    "roles/artifactregistry.reader",
    "roles/logging.logWriter",
    "roles/monitoring.metricWriter",
    "roles/monitoring.viewer",
    "roles/stackdriver.resourceMetadata.writer",
  ])
  project = var.project_id
  role    = each.value
  member  = "serviceAccount:${google_service_account.nodes.email}"
}

resource "google_container_cluster" "this" {
  name     = var.name
  location = var.region

  enable_autopilot    = true
  deletion_protection = var.deletion_protection

  network    = google_compute_network.this.id
  subnetwork = google_compute_subnetwork.this.id
  ip_allocation_policy {
    cluster_secondary_range_name  = "pods"
    services_secondary_range_name = "services"
  }

  cluster_autoscaling {
    auto_provisioning_defaults {
      service_account = google_service_account.nodes.email
      oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]
    }
  }

  release_channel {
    channel = "REGULAR"
  }

  depends_on = [google_project_service.apis, google_project_iam_member.nodes]
}
