provider "azurerm" {
  subscription_id = var.subscription

  # Deleting the resource group stops when it holds what OpenTofu didn't make (a disk the
  # CSI driver created and nobody deleted): tow deletes those through Kubernetes first.
  features {}
}

data "azurerm_client_config" "this" {}

locals {
  tags = merge(var.tags, {
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

  # A location without availability zones: nothing goes in a zone.
  zoned = !contains(var.zones, "")

  # The operating systems var.os offers, by CPU architecture: Canonical's and the AlmaLinux
  # Foundation's own Marketplace images, free and with no purchase plan, so no terms to accept
  # first (az vm image show --urn almalinux:almalinux-x86_64:10-gen2:latest: "plan": null).
  # AlmaLinux's community gallery isn't offered everywhere (not in westeurope).
  images = {
    "ubuntu-24.04" = {
      amd64 = { publisher = "Canonical", offer = "ubuntu-24_04-lts", sku = "server" }
      arm64 = { publisher = "Canonical", offer = "ubuntu-24_04-lts", sku = "server-arm64" }
    }
    "almalinux-9" = {
      amd64 = { publisher = "almalinux", offer = "almalinux-x86_64", sku = "9-gen2" }
      arm64 = { publisher = "almalinux", offer = "almalinux-arm", sku = "9-arm-gen2" }
    }
    "almalinux-10" = {
      amd64 = { publisher = "almalinux", offer = "almalinux-x86_64", sku = "10-gen2" }
      arm64 = { publisher = "almalinux", offer = "almalinux-arm", sku = "10-arm64-gen2" }
    }
  }
  image = local.images[var.os]

  # Test runs: Azure's auto-shutdown deallocates the machines (compute stops billing) at the
  # expiry time, as UTC hhmm.
  expires       = lookup(var.tags, "tow-expires", "")
  shutdown_time = local.expires != "" ? formatdate("hhmm", timeadd("1970-01-01T00:00:00Z", "${local.expires}s")) : ""
}

resource "azurerm_resource_group" "this" {
  name     = "tow-${var.cluster_name}"
  location = var.region
  tags     = local.tags
}

# Network: one VNet and one subnet (both regional, every zone shares them). The machines have
# public addresses: no NAT gateway, which would bill by the hour.

resource "azurerm_virtual_network" "this" {
  name                = "${var.cluster_name}-vnet"
  resource_group_name = azurerm_resource_group.this.name
  location            = var.region
  address_space       = [var.subnet_cidr]
  tags                = local.tags
}

resource "azurerm_subnet" "nodes" {
  name                 = "nodes"
  resource_group_name  = azurerm_resource_group.this.name
  virtual_network_name = azurerm_virtual_network.this.name
  address_prefixes     = [var.subnet_cidr]
}

# Firewall: Azure's default rules already let the VNet talk to itself and the load balancers'
# health probes in, and keep the internet out. From outside, only admin_cidrs reach SSH, the
# API, NodePort Services and the open ports. A rule takes every admin address at once, and
# changes in place.

resource "azurerm_network_security_group" "nodes" {
  name                = "${var.cluster_name}-nodes"
  resource_group_name = azurerm_resource_group.this.name
  location            = var.region
  tags                = local.tags

  security_rule {
    name                       = "admin-tcp"
    priority                   = 100
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_address_prefixes    = var.admin_cidrs
    source_port_range          = "*"
    destination_address_prefix = "*"
    destination_port_ranges    = concat(["22", "6443", "30000-32767"], [for p in var.open_ports : tostring(p)])
  }

  security_rule {
    name                       = "admin-udp"
    priority                   = 110
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Udp"
    source_address_prefixes    = var.admin_cidrs
    source_port_range          = "*"
    destination_address_prefix = "*"
    destination_port_range     = "30000-32767"
  }
}

resource "azurerm_subnet_network_security_group_association" "nodes" {
  subnet_id                 = azurerm_subnet.nodes.id
  network_security_group_id = azurerm_network_security_group.nodes.id
}

# The Azure Disk CSI driver's permissions (disk_csi): a managed identity the control planes
# carry, no keys anywhere, Contributor on the cluster's resource group only (its disks go
# there, and it attaches them to the cluster's machines there).

resource "azurerm_user_assigned_identity" "disk_csi" {
  count               = var.disk_csi ? 1 : 0
  name                = "${var.cluster_name}-disk-csi"
  resource_group_name = azurerm_resource_group.this.name
  location            = var.region
  tags                = local.tags
}

resource "azurerm_role_assignment" "disk_csi" {
  count                = var.disk_csi ? 1 : 0
  scope                = azurerm_resource_group.this.id
  role_definition_name = "Contributor"
  principal_id         = azurerm_user_assigned_identity.disk_csi[0].principal_id
  principal_type       = "ServicePrincipal"
}

# Machines: each with a public address of its own (static: it stays when the machine is
# deallocated and started again) and a network interface.

resource "azurerm_public_ip" "node" {
  for_each            = local.nodes
  name                = each.key
  resource_group_name = azurerm_resource_group.this.name
  location            = var.region
  allocation_method   = "Static"
  sku                 = "Standard"
  zones               = each.value.zone != "" ? [each.value.zone] : null
  tags                = local.tags
}

resource "azurerm_network_interface" "node" {
  for_each            = local.nodes
  name                = each.key
  resource_group_name = azurerm_resource_group.this.name
  location            = var.region
  tags                = local.tags

  ip_configuration {
    name                          = "primary"
    subnet_id                     = azurerm_subnet.nodes.id
    private_ip_address_allocation = "Dynamic"
    public_ip_address_id          = azurerm_public_ip.node[each.key].id
  }
}

resource "azurerm_linux_virtual_machine" "node" {
  for_each              = local.nodes
  name                  = each.key
  resource_group_name   = azurerm_resource_group.this.name
  location              = var.region
  size                  = each.value.instance_type
  zone                  = each.value.zone != "" ? each.value.zone : null
  network_interface_ids = [azurerm_network_interface.node[each.key].id]
  tags                  = merge(local.tags, { "tow-role" = each.value.role })

  # Azure creates ssh_user with the key and passwordless sudo; no password at all.
  admin_username                  = var.ssh_user
  disable_password_authentication = true
  admin_ssh_key {
    username   = var.ssh_user
    public_key = var.ssh_public_key
  }

  os_disk {
    caching              = "ReadWrite"
    storage_account_type = "StandardSSD_LRS"
    disk_size_gb         = each.value.disk_gb
  }

  source_image_reference {
    publisher = local.image[var.machine_type_archs[each.value.instance_type]].publisher
    offer     = local.image[var.machine_type_archs[each.value.instance_type]].offer
    sku       = local.image[var.machine_type_archs[each.value.instance_type]].sku
    version   = "latest"
  }

  # The Azure Disk CSI driver's identity, on the control planes only, where its controller
  # runs. Workers carry none.
  dynamic "identity" {
    for_each = var.disk_csi && each.value.role == "control-plane" ? [1] : []
    content {
      type         = "UserAssigned"
      identity_ids = [azurerm_user_assigned_identity.disk_csi[0].id]
    }
  }

  lifecycle {
    # A newer image goes to new machines; the ones running keep theirs.
    ignore_changes = [source_image_reference]
  }

  depends_on = [azurerm_role_assignment.disk_csi, azurerm_subnet_network_security_group_association.nodes]
}

resource "azurerm_dev_test_global_vm_shutdown_schedule" "node" {
  for_each              = local.shutdown_time != "" ? local.nodes : {}
  virtual_machine_id    = azurerm_linux_virtual_machine.node[each.key].id
  location              = var.region
  enabled               = true
  daily_recurrence_time = local.shutdown_time
  timezone              = "UTC"
  tags                  = local.tags

  notification_settings {
    enabled = false
  }
}

# API load balancers (HA clusters only): two Standard load balancers in front of the control
# planes, one per kind of client (an Azure load balancer has public or private frontends, not
# both).
#
#   - "api", public, for the user (kubectl): it keeps the caller's address, so the NSG lets in
#     admin_cidrs only, as with one control plane.
#   - "api_nodes", internal, for the nodes: its address is private, so their API calls stay
#     inside the VNet.
#
# A machine behind an internal load balancer can't reach it when Azure hands its connection
# back to itself, so the nodes reach the API by a name (api_node_name), which each machine
# resolves in /etc/hosts: a worker to the internal load balancer, a control plane to itself.
# Both probe the API servers every 5 s (kubeadm's docs: "a TCP check on the port the
# kube-apiserver listens on"). The machines keep their own public addresses for what they
# send out: no outbound rule.

locals {
  api_load_balancers = local.load_balancer ? {
    api       = { name = "${var.cluster_name}-api", internal = false }
    api_nodes = { name = "${var.cluster_name}-api-nodes", internal = true }
  } : {}
  api_name = "api.${var.cluster_name}.internal"
  frontend = "api"
}

resource "azurerm_public_ip" "api" {
  count               = local.load_balancer ? 1 : 0
  name                = "${var.cluster_name}-api"
  resource_group_name = azurerm_resource_group.this.name
  location            = var.region
  allocation_method   = "Static"
  sku                 = "Standard"
  zones               = local.zoned ? var.zones : null
  tags                = local.tags
}

resource "azurerm_lb" "api" {
  for_each            = local.api_load_balancers
  name                = each.value.name
  resource_group_name = azurerm_resource_group.this.name
  location            = var.region
  sku                 = "Standard"
  tags                = local.tags

  frontend_ip_configuration {
    name                          = local.frontend
    public_ip_address_id          = each.value.internal ? null : azurerm_public_ip.api[0].id
    subnet_id                     = each.value.internal ? azurerm_subnet.nodes.id : null
    private_ip_address_allocation = each.value.internal ? "Dynamic" : null
    zones                         = each.value.internal && local.zoned ? var.zones : null
  }
}

resource "azurerm_lb_backend_address_pool" "api" {
  for_each        = local.api_load_balancers
  name            = "control-planes"
  loadbalancer_id = azurerm_lb.api[each.key].id
}

resource "azurerm_lb_probe" "api" {
  for_each            = local.api_load_balancers
  name                = "api"
  loadbalancer_id     = azurerm_lb.api[each.key].id
  protocol            = "Tcp"
  port                = 6443
  interval_in_seconds = 5
  number_of_probes    = 2
}

resource "azurerm_lb_rule" "api" {
  for_each                       = local.api_load_balancers
  name                           = "api"
  loadbalancer_id                = azurerm_lb.api[each.key].id
  protocol                       = "Tcp"
  frontend_port                  = 6443
  backend_port                   = 6443
  frontend_ip_configuration_name = local.frontend
  backend_address_pool_ids       = [azurerm_lb_backend_address_pool.api[each.key].id]
  probe_id                       = azurerm_lb_probe.api[each.key].id
  disable_outbound_snat          = true
}

resource "azurerm_network_interface_backend_address_pool_association" "api" {
  for_each = { for pair in setproduct(keys(local.api_load_balancers), local.control_planes) : "${pair[0]}-${pair[1]}" => {
    load_balancer = pair[0]
    node          = pair[1]
  } }
  network_interface_id    = azurerm_network_interface.node[each.value.node].id
  ip_configuration_name   = "primary"
  backend_address_pool_id = azurerm_lb_backend_address_pool.api[each.value.load_balancer].id
}
