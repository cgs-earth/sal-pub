# Generated once and kept in Terraform state (keep the state bucket private).
resource "random_password" "db" {
  length  = 32
  special = false
}

resource "random_password" "keycloak_admin" {
  length  = 24
  special = false
}

resource "random_password" "client_secret" {
  length  = 40
  special = false
}

resource "random_id" "session_secret" {
  byte_length = 32
}

resource "kubernetes_secret_v1" "salpub" {
  metadata {
    name      = "salpub"
    namespace = kubernetes_namespace_v1.this.metadata[0].name
  }
  data = {
    DATABASE_URL           = "postgres://${var.db_username}:${random_password.db.result}@${google_sql_database_instance.this.private_ip_address}:5432/${google_sql_database.salpub.name}?sslmode=disable"
    KEYCLOAK_CLIENT_SECRET = random_password.client_secret.result
    SESSION_SECRET         = random_id.session_secret.hex
  }
}

resource "kubernetes_secret_v1" "keycloak" {
  metadata {
    name      = "keycloak"
    namespace = kubernetes_namespace_v1.this.metadata[0].name
  }
  data = merge(var.identity_providers, {
    KC_DB_USERNAME              = var.db_username
    KC_DB_PASSWORD              = random_password.db.result
    KC_BOOTSTRAP_ADMIN_USERNAME = var.keycloak_admin_username
    KC_BOOTSTRAP_ADMIN_PASSWORD = random_password.keycloak_admin.result
    # Substituted into the sal-pub client in the realm import (sal.json).
    KEYCLOAK_CLIENT_SECRET = random_password.client_secret.result
  })
}
