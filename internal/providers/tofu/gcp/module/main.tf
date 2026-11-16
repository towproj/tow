provider "google" {
  project = var.project
  region  = var.region

  # Every resource that takes labels gets the cluster's: delete uses them (and the names below,
  # for what takes none: networks, firewall rules, health checks, backend services, instance
  # groups) to prove nothing is left behind.
  default_labels = local.labels
}

locals {
  labels = merge(var.tags, {
    "tow-cluster"    = var.cluster_name
    "tow-managed-by" = "tow"
  })

  # Machines by name: each keeps its place in OpenTofu's state whatever else comes or goes.
  nodes = { for m in var.machines : m.name => merge(m, {
    instance_type = m.instance_type != "" ? m.instance_type : var.default_instance_type
    disk_gb       = m.disk_gb > 0 ? m.disk_gb : var.root_volume_gb
  }) }
  control_planes = [for m in var.machines : m.name if m.role == "control-plane"]

  # Only HA clusters need a load balancer in front of the API servers.
  load_balancer = length(local.control_planes) > 1

  # The operating systems var.os offers: the public image project and family of each, by CPU
  # architecture, in the publisher's own words (AlmaLinux's wiki: "All image families live in
  # the almalinux-cloud project"). Free images: no license fee.
  images = {
    "ubuntu-24.04" = { project = "ubuntu-os-cloud", family = { amd64 = "ubuntu-2404-lts-amd64", arm64 = "ubuntu-2404-lts-arm64" } }
    "almalinux-9"  = { project = "almalinux-cloud", family = { amd64 = "almalinux-9", arm64 = "almalinux-9-arm64" } }
    "almalinux-10" = { project = "almalinux-cloud", family = { amd64 = "almalinux-10", arm64 = "almalinux-10-arm64" } }
  }
  image = local.images[var.os]
}

# The newest image of the system, for each CPU architecture the machines need. Machines that
# run keep the image they were made from (ignore_changes below).
data "google_compute_image" "image" {
  for_each = toset(values(var.machine_type_archs))
  project  = local.image.project
  family   = local.image.family[each.value]
}

# Network: one VPC per cluster, one subnet (subnets are regional on GCP, every zone shares it).
# The machines have public addresses: no Cloud NAT, which would bill by the hour.

resource "google_compute_network" "this" {
  name                    = "${var.cluster_name}-vpc"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "nodes" {
  name          = "${var.cluster_name}-nodes"
  region        = var.region
  network       = google_compute_network.this.id
  ip_cidr_range = var.subnet_cidr
}

# Firewall: nodes talk freely to each other; from outside, only admin_cidrs reach SSH, the API,
# NodePort Services and the open ports. A rule takes every admin address at once, and changes
# in place when they change.

resource "google_compute_firewall" "internal" {
  name          = "${var.cluster_name}-internal"
  network       = google_compute_network.this.id
  source_ranges = [var.subnet_cidr]

  allow {
    protocol = "all"
  }
}

resource "google_compute_firewall" "admin" {
  name          = "${var.cluster_name}-admin"
  network       = google_compute_network.this.id
  source_ranges = var.admin_cidrs

  allow {
    protocol = "tcp"
    ports    = concat(["22", "6443", "30000-32767"], [for p in var.open_ports : tostring(p)])
  }
  allow {
    protocol = "udp"
    ports    = ["30000-32767"]
  }
}

# Google's health checkers reach the API servers from their own ranges, for both load
# balancers: https://cloud.google.com/load-balancing/docs/health-check-concepts#ip-ranges
resource "google_compute_firewall" "health_checks" {
  count         = local.load_balancer ? 1 : 0
  name          = "${var.cluster_name}-health"
  network       = google_compute_network.this.id
  source_ranges = ["35.191.0.0/16", "130.211.0.0/22", "209.85.152.0/22", "209.85.204.0/22"]

  allow {
    protocol = "tcp"
    ports    = ["6443"]
  }
}

# The GCE PD CSI driver's permissions (pd_csi): a service account the control planes carry, no
# keys anywhere. Its upstream setup grants roles/editor; this is narrower: disks (Compute
# Storage Admin), attaching them to the cluster's machines (the driver's own custom role,
# deploy/gcp-compute-persistent-disk-csi-driver-custom-role.yaml, plus reading operations),
# and acting as the account the control planes run as, on that account only (attaching a disk
# to a machine that runs as a service account asks for it).

resource "google_service_account" "pd_csi" {
  count        = var.pd_csi ? 1 : 0
  account_id   = "${substr(var.cluster_name, 0, 24)}-pdcsi"
  display_name = "tow ${var.cluster_name}: GCE PD CSI driver"
}

resource "google_project_iam_custom_role" "pd_csi" {
  count       = var.pd_csi ? 1 : 0
  role_id     = "tow_${replace(var.cluster_name, "-", "_")}_pdcsi"
  title       = "tow ${var.cluster_name}: GCE PD CSI driver"
  permissions = ["compute.instances.get", "compute.instances.attachDisk", "compute.instances.detachDisk", "compute.disks.get", "compute.zoneOperations.get", "compute.regionOperations.get"]
}

resource "google_project_iam_member" "pd_csi" {
  for_each = var.pd_csi ? { storage = "roles/compute.storageAdmin", attach = google_project_iam_custom_role.pd_csi[0].name } : {}
  project  = var.project
  role     = each.value
  member   = "serviceAccount:${google_service_account.pd_csi[0].email}"
}

resource "google_service_account_iam_member" "pd_csi" {
  count              = var.pd_csi ? 1 : 0
  service_account_id = google_service_account.pd_csi[0].name
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${google_service_account.pd_csi[0].email}"
}

# Machines

resource "google_compute_instance" "node" {
  for_each     = local.nodes
  name         = each.key
  zone         = each.value.zone
  machine_type = each.value.instance_type
  labels       = { "tow-role" = each.value.role }

  boot_disk {
    auto_delete = true
    initialize_params {
      image  = data.google_compute_image.image[var.machine_type_archs[each.value.instance_type]].self_link
      size   = each.value.disk_gb
      type   = "pd-balanced"
      labels = merge(local.labels, { "tow-role" = each.value.role })
    }
  }

  # A public address of its own (ephemeral: a stopped machine gets a new one when it starts).
  network_interface {
    subnetwork = google_compute_subnetwork.nodes.id
    access_config {}
  }

  # Google's guest agent creates ssh_user from the key, with passwordless sudo. Keys from the
  # project's metadata and OS Login stay out: only tow's key logs in.
  metadata = {
    "ssh-keys"               = "${var.ssh_user}:${var.ssh_public_key}"
    "block-project-ssh-keys" = "TRUE"
    "enable-oslogin"         = "FALSE"
  }

  # The PD CSI driver's account, on the control planes only, where its controller runs.
  # Workers run with none.
  dynamic "service_account" {
    for_each = var.pd_csi && each.value.role == "control-plane" ? [1] : []
    content {
      email  = google_service_account.pd_csi[0].email
      scopes = ["cloud-platform"]
    }
  }

  # Test runs only: Compute Engine deletes the machine and its disk even if 'tow delete'
  # never runs.
  dynamic "scheduling" {
    for_each = var.self_destruct_minutes > 0 ? [1] : []
    content {
      instance_termination_action = "DELETE"
      max_run_duration {
        seconds = var.self_destruct_minutes * 60
      }
    }
  }

  lifecycle {
    # A newer image goes to new machines; the ones running keep theirs.
    ignore_changes = [boot_disk[0].initialize_params[0].image]
  }

  depends_on = [google_project_iam_member.pd_csi, google_service_account_iam_member.pd_csi]
}

# API load balancers (HA clusters only): two passthrough network load balancers in front of
# the control planes, one per kind of client.
#
#   - "api", external, for the user (kubectl): it keeps the caller's address, so the "admin"
#     firewall rule lets in admin_cidrs only, as with one control plane.
#   - "api_nodes", internal, for the nodes: its address is private, so their API calls stay
#     inside the VPC.
#
# A machine behind them can't use them: Google's guest agent routes a load balancer's address
# to the machine itself (so the machine accepts what the load balancer passes on), and a
# control plane joining the cluster would reach its own API server, not running yet. So the
# nodes reach the API by a name (api_node_name), which each machine resolves in /etc/hosts: a
# worker to the internal load balancer, a control plane to itself.
#
# Both health-check the API servers every 10 s (kubeadm's docs: "a TCP check on the port the
# kube-apiserver listens on"), so a server that stops is out within ~20 s. Each control plane
# is the one member of its own instance group, named after it, so adding or removing one
# touches only its own.

locals {
  api_load_balancers = local.load_balancer ? {
    api       = { name = "${var.cluster_name}-api", scheme = "EXTERNAL" }
    api_nodes = { name = "${var.cluster_name}-api-nodes", scheme = "INTERNAL" }
  } : {}
  api_name = "api.${var.cluster_name}.internal"
}

resource "google_compute_instance_group" "api" {
  for_each  = toset(local.load_balancer ? local.control_planes : [])
  name      = each.key
  zone      = local.nodes[each.key].zone
  instances = [google_compute_instance.node[each.key].self_link]
}

resource "google_compute_region_health_check" "api" {
  for_each            = local.api_load_balancers
  name                = each.value.name
  region              = var.region
  check_interval_sec  = 10
  timeout_sec         = 5
  healthy_threshold   = 2
  unhealthy_threshold = 2

  tcp_health_check {
    port = 6443
  }
}

resource "google_compute_region_backend_service" "api" {
  for_each              = local.api_load_balancers
  name                  = each.value.name
  region                = var.region
  protocol              = "TCP"
  load_balancing_scheme = each.value.scheme
  health_checks         = [google_compute_region_health_check.api[each.key].id]

  dynamic "backend" {
    for_each = google_compute_instance_group.api
    content {
      group          = backend.value.id
      balancing_mode = "CONNECTION"
    }
  }
}

resource "google_compute_address" "api" {
  for_each     = local.api_load_balancers
  name         = each.value.name
  region       = var.region
  address_type = each.value.scheme
  subnetwork   = each.value.scheme == "INTERNAL" ? google_compute_subnetwork.nodes.id : null
}

resource "google_compute_forwarding_rule" "api" {
  for_each              = local.api_load_balancers
  name                  = each.value.name
  region                = var.region
  load_balancing_scheme = each.value.scheme
  backend_service       = google_compute_region_backend_service.api[each.key].id
  ip_address            = google_compute_address.api[each.key].address
  ip_protocol           = "TCP"
  ports                 = ["6443"]
  network               = each.value.scheme == "INTERNAL" ? google_compute_network.this.id : null
  subnetwork            = each.value.scheme == "INTERNAL" ? google_compute_subnetwork.nodes.id : null
}
