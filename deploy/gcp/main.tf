locals {
  public_url = "https://${var.hostname}"

  registry = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.images.repository_id}"
  images = {
    salpub   = "${local.registry}/salpub:${var.image_tag}"
    keycloak = "${local.registry}/keycloak:${var.image_tag}"
    caddy    = "${local.registry}/caddy:${var.image_tag}"
  }
}

resource "google_project_service" "apis" {
  for_each = toset([
    "artifactregistry.googleapis.com",
    "compute.googleapis.com",
    "container.googleapis.com",
    "servicenetworking.googleapis.com",
    "sqladmin.googleapis.com",
  ])
  service            = each.value
  disable_on_destroy = false
}

resource "google_artifact_registry_repository" "images" {
  repository_id = var.name
  location      = var.region
  format        = "DOCKER"
  description   = "sal-pub, Keycloak and Caddy images"
  depends_on    = [google_project_service.apis]
}
