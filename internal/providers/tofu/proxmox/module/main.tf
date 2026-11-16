# The API token comes from PROXMOX_VE_API_TOKEN. TLS is verified unless PROXMOX_VE_INSECURE
# is set (self-signed lab certificates); tow warns every time it is.
provider "proxmox" {
  endpoint = var.proxmox_url
}

locals {
  # VMs by name: each keeps its place in OpenTofu's state whatever else comes or goes.
  nodes = { for m in var.machines : m.name => m }

  # Proxmox tags are single words, so key=value becomes key.value. Sorted, because Proxmox
  # stores them sorted and a different order would show up as a change on every plan.
  base_tags = concat(
    ["tow", "tow-cluster.${var.cluster_name}"],
    [for k, v in var.tags : "${k}.${v}"],
  )

  # Nodes reach each other on any port; the virtual IP moves between control planes; and pods'
  # traffic a network routes unwrapped keeps the pod's address (as ufw allows on own machines).
  cluster_sources = join(",", concat(
    [for m in var.machines : m.address],
    var.api_endpoint == "" ? [] : [split(":", var.api_endpoint)[0]],
    var.pod_cidr == "" ? [] : [var.pod_cidr],
  ))
}

resource "proxmox_virtual_environment_vm" "node" {
  for_each    = local.nodes
  name        = each.key
  node_name   = var.region
  description = "Managed by tow (cluster ${var.cluster_name}). Remove it with: tow delete --name ${var.cluster_name}"
  tags        = sort(concat(local.base_tags, ["tow-role.${each.value.role}"]))
  on_boot     = true

  clone {
    vm_id = var.template_id
    full  = true
  }

  # No guest agent in the stock cloud image: addresses are static, so nothing needs it.
  # Without it a graceful shutdown can hang, so destroy stops the VM instead.
  agent {
    enabled = false
  }
  stop_on_destroy = true

  # An update that needs the VM offline fails instead: tow never reboots a machine as a
  # side effect.
  reboot_after_update = false

  cpu {
    cores = each.value.cpus
    type  = var.cpu_type
  }

  memory {
    dedicated = each.value.memory_mb
  }

  disk {
    datastore_id = var.storage
    interface    = "scsi0"
    size         = each.value.disk_gb
    discard      = "on"
  }

  network_device {
    bridge   = var.bridge
    firewall = true
  }

  operating_system {
    type = "l26"
  }

  serial_device {}

  initialization {
    datastore_id = var.storage

    ip_config {
      ipv4 {
        address = "${each.value.address}/${var.prefix_length}"
        gateway = var.gateway
      }
    }

    dns {
      servers = var.dns_servers
    }

    user_account {
      username = var.ssh_user
      keys     = [trimspace(var.ssh_public_key)]
    }
  }

  lifecycle {
    # The template only matters when a VM is created: new VMs are cloned from the file's
    # template, and the ones running keep what they were cloned from.
    ignore_changes = [clone]
  }
}

# Firewall: nodes talk freely to each other; from outside, only admin_cidrs reach SSH, the API
# and NodePort Services. Proxmox enforces these only while the Datacenter firewall is on;
# tow warns otherwise.

resource "proxmox_virtual_environment_firewall_options" "node" {
  for_each      = proxmox_virtual_environment_vm.node
  node_name     = var.region
  vm_id         = each.value.vm_id
  enabled       = true
  input_policy  = "DROP"
  output_policy = "ACCEPT"
}

resource "proxmox_virtual_environment_firewall_rules" "node" {
  for_each  = proxmox_virtual_environment_vm.node
  node_name = var.region
  vm_id     = each.value.vm_id

  rule {
    type    = "in"
    action  = "ACCEPT"
    source  = local.cluster_sources
    comment = "tow: cluster nodes"
  }

  dynamic "rule" {
    for_each = var.admin_cidrs
    content {
      type    = "in"
      action  = "ACCEPT"
      proto   = "tcp"
      dport   = "22"
      source  = rule.value
      comment = "tow: SSH from admin"
    }
  }

  dynamic "rule" {
    for_each = var.admin_cidrs
    content {
      type    = "in"
      action  = "ACCEPT"
      proto   = "tcp"
      dport   = "6443"
      source  = rule.value
      comment = "tow: Kubernetes API from admin"
    }
  }

  dynamic "rule" {
    for_each = setproduct(var.admin_cidrs, ["tcp", "udp"])
    content {
      type    = "in"
      action  = "ACCEPT"
      proto   = rule.value[1]
      dport   = "30000:32767"
      source  = rule.value[0]
      comment = "tow: NodePort Services from admin"
    }
  }

  # The cluster's ingress (80, 443) and the file's access.ports.
  dynamic "rule" {
    for_each = setproduct(var.admin_cidrs, var.open_ports)
    content {
      type    = "in"
      action  = "ACCEPT"
      proto   = "tcp"
      dport   = tostring(rule.value[1])
      source  = rule.value[0]
      comment = "tow: port ${rule.value[1]} from admin"
    }
  }

  # The node that announces a LoadBalancer Service's address receives its traffic.
  dynamic "rule" {
    for_each = setproduct(var.admin_cidrs, var.service_lb_cidrs)
    content {
      type    = "in"
      action  = "ACCEPT"
      source  = rule.value[0]
      dest    = rule.value[1]
      comment = "tow: LoadBalancer Services from admin"
    }
  }
}
