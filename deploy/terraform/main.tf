# Terraform for MIRROR's application layer on an EXISTING Kubernetes cluster.
#
# This deliberately does NOT provision a cloud Kubernetes cluster (GKE/EKS/AKS).
# Doing that convincingly means committing to one cloud provider's networking,
# IAM and node-pool model, none of which this project has actually run against
# in production -- and "Terraform that has never applied cleanly against a
# real account" is worse than no Terraform, per this project's own rule that
# complexity must be earned and demonstrated, not aspirational.
#
# What this DOES manage: the Kubernetes-native resources in ../k8s, as
# Terraform resources instead of kubectl apply, so that applying this module
# against any cluster (a local kind/minikube cluster, or a real one you point
# it at via KUBECONFIG) gives you a working MIRROR deployment. Point
# `kubernetes` provider config (via the standard KUBECONFIG env var, or the
# variables below) at whatever cluster you have.

terraform {
  required_version = ">= 1.5"
  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.31"
    }
  }
}

variable "namespace" {
  description = "Kubernetes namespace to deploy MIRROR into."
  type        = string
  default     = "mirror"
}

variable "image" {
  description = "Container image for the mirrord binary (see ../docker/Dockerfile)."
  type        = string
  default     = "mirror:latest"
}

variable "api_keys" {
  description = "MIRROR_API_KEYS value: comma-separated name:role:secret entries. Pass via -var or TF_VAR_api_keys, never commit a real value."
  type        = string
  sensitive   = true
}

variable "storage_class" {
  description = "StorageClass for the checkpoint volume. Empty string uses the cluster default."
  type        = string
  default     = ""
}

provider "kubernetes" {
  # Configured via KUBECONFIG by default. Override here if this module needs
  # to target a specific context in CI.
}

resource "kubernetes_namespace" "mirror" {
  metadata {
    name = var.namespace
  }
}

resource "kubernetes_secret" "api_keys" {
  metadata {
    name      = "mirror-api-keys"
    namespace = kubernetes_namespace.mirror.metadata[0].name
  }
  data = {
    keys = var.api_keys
  }
  type = "Opaque"
}

resource "kubernetes_stateful_set" "mirror" {
  metadata {
    name      = "mirror"
    namespace = kubernetes_namespace.mirror.metadata[0].name
    labels    = { "app.kubernetes.io/name" = "mirror" }
  }

  spec {
    service_name = "mirror"
    replicas     = 1

    selector {
      match_labels = { "app.kubernetes.io/name" = "mirror" }
    }

    template {
      metadata {
        labels = { "app.kubernetes.io/name" = "mirror" }
      }
      spec {
        security_context {
          run_as_non_root = true
          fs_group        = 65532
        }
        container {
          name  = "mirrord"
          image = var.image

          port {
            name           = "http"
            container_port = 8080
          }

          env {
            name  = "MIRROR_AUTH_MODE"
            value = "production"
          }
          env {
            name = "MIRROR_API_KEYS"
            value_from {
              secret_key_ref {
                name = kubernetes_secret.api_keys.metadata[0].name
                key  = "keys"
              }
            }
          }

          resources {
            requests = { cpu = "1", memory = "512Mi" }
            limits   = { cpu = "4", memory = "2Gi" }
          }

          volume_mount {
            name       = "data"
            mount_path = "/data"
          }

          readiness_probe {
            http_get {
              path = "/readyz"
              port = "http"
            }
            initial_delay_seconds = 3
            period_seconds        = 5
          }
          liveness_probe {
            http_get {
              path = "/healthz"
              port = "http"
            }
            initial_delay_seconds = 5
            period_seconds        = 10
          }
        }
      }
    }

    volume_claim_template {
      metadata {
        name = "data"
      }
      spec {
        access_modes       = ["ReadWriteOnce"]
        storage_class_name = var.storage_class != "" ? var.storage_class : null
        resources {
          requests = { storage = "10Gi" }
        }
      }
    }
  }
}

resource "kubernetes_service" "mirror" {
  metadata {
    name      = "mirror"
    namespace = kubernetes_namespace.mirror.metadata[0].name
    labels    = { "app.kubernetes.io/name" = "mirror" }
  }
  spec {
    cluster_ip = "None" # headless: stable per-pod DNS for sticky sessions
    selector   = { "app.kubernetes.io/name" = "mirror" }
    port {
      name        = "http"
      port        = 8080
      target_port = "http"
    }
  }
}

output "service_dns" {
  value       = "${kubernetes_service.mirror.metadata[0].name}-0.${kubernetes_service.mirror.metadata[0].name}.${kubernetes_namespace.mirror.metadata[0].name}.svc.cluster.local"
  description = "In-cluster DNS name of the first (and, currently, only) MIRROR pod."
}
