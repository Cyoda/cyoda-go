terraform {
  required_version = ">= 1.6"
  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 3.112"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.31"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 2.14"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}

provider "azurerm" {
  subscription_id = var.subscription_id
  features {
    resource_group {
      prevent_deletion_if_contains_resources = false
    }
    key_vault {
      purge_soft_delete_on_destroy    = true
      recover_soft_deleted_key_vaults = true
    }
  }
}

# ── Resource Group ───────────────────────────────────────────────────────────

resource "azurerm_resource_group" "main" {
  name     = var.resource_group_name
  location = var.location
}

# ── Virtual Network ──────────────────────────────────────────────────────────

resource "azurerm_virtual_network" "main" {
  name                = "${var.cluster_name}-vnet"
  resource_group_name = azurerm_resource_group.main.name
  location            = azurerm_resource_group.main.location
  address_space       = ["10.0.0.0/8"]
}

resource "azurerm_subnet" "aks" {
  name                 = "${var.cluster_name}-aks-subnet"
  resource_group_name  = azurerm_resource_group.main.name
  virtual_network_name = azurerm_virtual_network.main.name
  address_prefixes     = ["10.1.0.0/16"]
}

resource "azurerm_subnet" "postgres" {
  name                 = "${var.cluster_name}-postgres-subnet"
  resource_group_name  = azurerm_resource_group.main.name
  virtual_network_name = azurerm_virtual_network.main.name
  address_prefixes     = ["10.2.0.0/24"]

  delegation {
    name = "postgres-delegation"
    service_delegation {
      name    = "Microsoft.DBforPostgreSQL/flexibleServers"
      actions = ["Microsoft.Network/virtualNetworks/subnets/join/action"]
    }
  }
}

# ── DNS zone for PostgreSQL private DNS ─────────────────────────────────────

resource "azurerm_private_dns_zone" "postgres" {
  name                = "${var.cluster_name}.postgres.database.azure.com"
  resource_group_name = azurerm_resource_group.main.name
}

resource "azurerm_private_dns_zone_virtual_network_link" "postgres" {
  name                  = "${var.cluster_name}-pg-dns-link"
  resource_group_name   = azurerm_resource_group.main.name
  private_dns_zone_name = azurerm_private_dns_zone.postgres.name
  virtual_network_id    = azurerm_virtual_network.main.id
  registration_enabled  = false
}

# ── Azure Database for PostgreSQL Flexible Server ────────────────────────────

resource "azurerm_postgresql_flexible_server" "main" {
  name                          = "${var.cluster_name}-postgres"
  resource_group_name           = azurerm_resource_group.main.name
  location                      = azurerm_resource_group.main.location
  version                       = var.db_version
  delegated_subnet_id           = azurerm_subnet.postgres.id
  private_dns_zone_id           = azurerm_private_dns_zone.postgres.id
  administrator_login           = var.db_admin_username
  administrator_password        = var.db_admin_password
  zone                          = var.db_zone
  sku_name                      = var.db_sku_name
  storage_mb                    = var.db_storage_mb
  backup_retention_days         = 7
  geo_redundant_backup_enabled  = false

  depends_on = [azurerm_private_dns_zone_virtual_network_link.postgres]
}

resource "azurerm_postgresql_flexible_server_database" "cyoda" {
  name      = var.db_name
  server_id = azurerm_postgresql_flexible_server.main.id
  collation = "en_US.utf8"
  charset   = "utf8"
}

# ── AKS Cluster ─────────────────────────────────────────────────────────────

resource "azurerm_kubernetes_cluster" "main" {
  name                = var.cluster_name
  resource_group_name = azurerm_resource_group.main.name
  location            = azurerm_resource_group.main.location
  dns_prefix          = var.cluster_name
  kubernetes_version  = var.kubernetes_version

  default_node_pool {
    name                = "default"
    vm_size             = var.node_vm_size
    node_count          = var.node_count
    min_count           = var.node_min_count
    max_count           = var.node_max_count
    auto_scaling_enabled = true
    vnet_subnet_id      = azurerm_subnet.aks.id
    os_disk_size_gb     = 50
  }

  identity {
    type = "SystemAssigned"
  }

  network_profile {
    network_plugin    = "azure"
    load_balancer_sku = "standard"
  }

  oms_agent {
    log_analytics_workspace_id = azurerm_log_analytics_workspace.main.id
  }
}

resource "azurerm_log_analytics_workspace" "main" {
  name                = "${var.cluster_name}-logs"
  resource_group_name = azurerm_resource_group.main.name
  location            = azurerm_resource_group.main.location
  sku                 = "PerGB2018"
  retention_in_days   = 30
}

# ── Kubernetes / Helm providers (post-cluster) ──────────────────────────────

provider "kubernetes" {
  host                   = azurerm_kubernetes_cluster.main.kube_config[0].host
  cluster_ca_certificate = base64decode(azurerm_kubernetes_cluster.main.kube_config[0].cluster_ca_certificate)
  client_certificate     = base64decode(azurerm_kubernetes_cluster.main.kube_config[0].client_certificate)
  client_key             = base64decode(azurerm_kubernetes_cluster.main.kube_config[0].client_key)
}

provider "helm" {
  kubernetes {
    host                   = azurerm_kubernetes_cluster.main.kube_config[0].host
    cluster_ca_certificate = base64decode(azurerm_kubernetes_cluster.main.kube_config[0].cluster_ca_certificate)
    client_certificate     = base64decode(azurerm_kubernetes_cluster.main.kube_config[0].client_certificate)
    client_key             = base64decode(azurerm_kubernetes_cluster.main.kube_config[0].client_key)
  }
}

# ── cyoda Helm release ───────────────────────────────────────────────────────

locals {
  postgres_host = azurerm_postgresql_flexible_server.main.fqdn
  postgres_dsn  = "postgres://${var.db_admin_username}:${var.db_admin_password}@${local.postgres_host}:5432/${var.db_name}?sslmode=require"
}

module "cyoda" {
  source = "../modules/cyoda-helm"

  namespace           = var.cyoda_namespace
  chart_path          = "../../helm/cyoda"
  image_tag           = var.cyoda_image_tag
  replicas            = var.cyoda_replicas
  postgres_dsn        = local.postgres_dsn
  jwt_signing_key_pem = var.jwt_signing_key_pem
  jwt_issuer          = var.jwt_issuer

  depends_on = [azurerm_kubernetes_cluster.main]
}
