variable "project_id" {
  description = "GCP project to deploy into."
  type        = string
}

variable "region" {
  description = "Region for the cluster, database, registry and static IP."
  type        = string
  default     = "us-central1"
}

variable "name" {
  description = "Prefix for resource names."
  type        = string
  default     = "salpub"
}

variable "hostname" {
  description = "Public hostname, without scheme (e.g. sal.example.org). Actor IDs are built from it, so don't change it once real users exist. Point its DNS A record at the static_ip output."
  type        = string
}

variable "image_tag" {
  description = "Tag of the salpub, keycloak and caddy images in Artifact Registry (pushed by build-images.sh)."
  type        = string
}

variable "db_tier" {
  description = "Cloud SQL machine tier."
  type        = string
  default     = "db-custom-1-3840"
}

variable "db_username" {
  description = "Postgres user shared by Keycloak and sal-pub."
  type        = string
  default     = "salpub"
}

variable "keycloak_admin_username" {
  description = "Bootstrap admin user for the Keycloak master realm. Its password is generated (see outputs)."
  type        = string
  default     = "admin"
}

variable "identity_providers" {
  description = "Identity-provider credentials substituted into the realm import, e.g. { IDP_GITHUB_CLIENT_ID = \"...\" }. Keys are the IDP_* variables from .env.default."
  type        = map(string)
  default     = {}
  sensitive   = true
}

variable "salpub_replicas" {
  description = "Number of sal-pub pods. Sessions are signed cookies, so any pod can serve any request."
  type        = number
  default     = 2
}

variable "deletion_protection" {
  description = "Protect the cluster and database from terraform destroy."
  type        = bool
  default     = true
}
