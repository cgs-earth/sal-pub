# One Postgres instance with two databases, as in docker-compose: "keycloak"
# for Keycloak and "salpub" for the server (which runs its own migrations).
resource "google_sql_database_instance" "this" {
  name                = var.name
  region              = var.region
  database_version    = "POSTGRES_17"
  deletion_protection = var.deletion_protection

  settings {
    tier              = var.db_tier
    edition           = "ENTERPRISE"
    availability_type = "ZONAL"

    ip_configuration {
      ipv4_enabled    = false # private IP only, reachable from the cluster's VPC
      private_network = google_compute_network.this.id
    }

    backup_configuration {
      enabled                        = true
      point_in_time_recovery_enabled = true
    }
  }

  depends_on = [google_service_networking_connection.private_services]
}

resource "google_sql_database" "keycloak" {
  name     = "keycloak"
  instance = google_sql_database_instance.this.name
}

resource "google_sql_database" "salpub" {
  name     = "salpub"
  instance = google_sql_database_instance.this.name
}

resource "google_sql_user" "app" {
  name     = var.db_username
  instance = google_sql_database_instance.this.name
  password = random_password.db.result
}
