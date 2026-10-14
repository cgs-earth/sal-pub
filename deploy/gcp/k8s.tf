# The same three services as docker-compose.yml. Service names matter:
# sal-pub reaches Keycloak at http://keycloak:8080 and Caddy proxies to
# keycloak:8080 and salpub:8000.

resource "kubernetes_namespace_v1" "this" {
  metadata {
    name = var.name
  }
}

# --- Keycloak ----------------------------------------------------------------

resource "kubernetes_deployment_v1" "keycloak" {
  metadata {
    name      = "keycloak"
    namespace = kubernetes_namespace_v1.this.metadata[0].name
  }
  spec {
    replicas = 1 # clustering isn't configured
    strategy {
      type = "Recreate"
    }
    selector {
      match_labels = { app = "keycloak" }
    }
    template {
      metadata {
        labels = { app = "keycloak" }
        annotations = {
          "checksum/secret" = sha256(jsonencode(kubernetes_secret_v1.keycloak.data))
        }
      }
      spec {
        container {
          name  = "keycloak"
          image = local.images.keycloak
          port {
            container_port = 8080
          }
          # Fixed settings come from docker/keycloak/Dockerfile; these override
          # or complete them for this deployment.
          env {
            name  = "KC_HOSTNAME"
            value = "${local.public_url}/auth"
          }
          env {
            name  = "SALPUB_HOSTNAME"
            value = local.public_url
          }
          env {
            name  = "KC_DB_URL_HOST"
            value = google_sql_database_instance.this.private_ip_address
          }
          env_from {
            secret_ref {
              name = kubernetes_secret_v1.keycloak.metadata[0].name
            }
          }
          resources {
            requests = { cpu = "1", memory = "2Gi" }
          }
          startup_probe {
            tcp_socket {
              port = 8080
            }
            period_seconds    = 5
            failure_threshold = 60 # first start builds and imports the realm
          }
          readiness_probe {
            tcp_socket {
              port = 8080
            }
            period_seconds = 10
          }
        }
      }
    }
  }
}

resource "kubernetes_service_v1" "keycloak" {
  metadata {
    name      = "keycloak"
    namespace = kubernetes_namespace_v1.this.metadata[0].name
  }
  spec {
    selector = { app = "keycloak" }
    port {
      port        = 8080
      target_port = 8080
    }
  }
}

# --- sal-pub -----------------------------------------------------------------

resource "kubernetes_deployment_v1" "salpub" {
  metadata {
    name      = "salpub"
    namespace = kubernetes_namespace_v1.this.metadata[0].name
  }
  spec {
    replicas = var.salpub_replicas
    selector {
      match_labels = { app = "salpub" }
    }
    template {
      metadata {
        labels = { app = "salpub" }
        annotations = {
          # Roll the pods when secrets change.
          "checksum/secret" = sha256(jsonencode(kubernetes_secret_v1.salpub.data))
        }
      }
      spec {
        container {
          name  = "salpub"
          image = local.images.salpub
          port {
            container_port = 8000
          }
          env {
            name  = "SALPUB_HOSTNAME"
            value = local.public_url
          }
          env_from {
            secret_ref {
              name = kubernetes_secret_v1.salpub.metadata[0].name
            }
          }
          resources {
            requests = { cpu = "250m", memory = "256Mi" }
          }
          # The server exits until Keycloak and Postgres are reachable;
          # Kubernetes restarts it, as Compose does.
          readiness_probe {
            http_get {
              path = "/"
              port = 8000
            }
            period_seconds = 10
          }
          liveness_probe {
            tcp_socket {
              port = 8000
            }
            initial_delay_seconds = 10
            period_seconds        = 20
          }
        }
      }
    }
  }
}

resource "kubernetes_service_v1" "salpub" {
  metadata {
    name      = "salpub"
    namespace = kubernetes_namespace_v1.this.metadata[0].name
  }
  spec {
    selector = { app = "salpub" }
    port {
      port        = 8000
      target_port = 8000
    }
  }
}

# --- Caddy (public entry point, TLS via Let's Encrypt) ----------------------

# Certificates and ACME account, so restarts don't re-issue certificates.
resource "kubernetes_persistent_volume_claim_v1" "caddy_data" {
  metadata {
    name      = "caddy-data"
    namespace = kubernetes_namespace_v1.this.metadata[0].name
  }
  spec {
    access_modes = ["ReadWriteOnce"]
    resources {
      requests = { storage = "1Gi" }
    }
  }
  wait_until_bound = false # binds when the Caddy pod is scheduled
}

resource "kubernetes_deployment_v1" "caddy" {
  metadata {
    name      = "caddy"
    namespace = kubernetes_namespace_v1.this.metadata[0].name
  }
  spec {
    replicas = 1 # one pod owns the certificate volume
    strategy {
      type = "Recreate"
    }
    selector {
      match_labels = { app = "caddy" }
    }
    template {
      metadata {
        labels = { app = "caddy" }
      }
      spec {
        container {
          name  = "caddy"
          image = local.images.caddy
          port {
            name           = "http"
            container_port = 80
          }
          port {
            name           = "https"
            container_port = 443
          }
          env {
            name  = "SALPUB_HOSTNAME"
            value = local.public_url
          }
          volume_mount {
            name       = "data"
            mount_path = "/data"
          }
          resources {
            requests = { cpu = "250m", memory = "256Mi" }
          }
          readiness_probe {
            tcp_socket {
              port = 443
            }
            period_seconds = 10
          }
        }
        volume {
          name = "data"
          persistent_volume_claim {
            claim_name = kubernetes_persistent_volume_claim_v1.caddy_data.metadata[0].name
          }
        }
      }
    }
  }
}

resource "kubernetes_service_v1" "caddy" {
  metadata {
    name      = "caddy"
    namespace = kubernetes_namespace_v1.this.metadata[0].name
  }
  spec {
    type             = "LoadBalancer"
    load_balancer_ip = google_compute_address.ingress.address
    # Keep client IPs (Caddy passes them on as X-Forwarded-For).
    external_traffic_policy = "Local"
    selector                = { app = "caddy" }
    port {
      name        = "http"
      port        = 80
      target_port = 80
    }
    port {
      name        = "https"
      port        = 443
      target_port = 443
    }
  }
}
