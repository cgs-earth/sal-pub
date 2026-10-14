output "static_ip" {
  description = "Create a DNS A record for var.hostname pointing here. Caddy gets a certificate once it resolves."
  value       = google_compute_address.ingress.address
}

output "url" {
  value = local.public_url
}

output "registry" {
  description = "Artifact Registry path that build-images.sh pushes to."
  value       = local.registry
}

output "get_credentials" {
  description = "Command to point kubectl at the cluster."
  value       = "gcloud container clusters get-credentials ${google_container_cluster.this.name} --region ${var.region} --project ${var.project_id}"
}

output "keycloak_admin_username" {
  value = var.keycloak_admin_username
}

output "keycloak_admin_password" {
  description = "Read with: tofu output -raw keycloak_admin_password"
  value       = random_password.keycloak_admin.result
  sensitive   = true
}
