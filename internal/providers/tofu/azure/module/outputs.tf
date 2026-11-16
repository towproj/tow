# Contract shared by every cloud module; tow reads exactly these.

output "nodes" {
  description = "Every machine, in the order of var.machines: name, role, pool, public_ip (SSH), private_ip (node-to-node), ssh_user."
  value = [for m in var.machines : {
    name       = m.name
    role       = m.role
    pool       = m.pool
    public_ip  = azurerm_public_ip.node[m.name].ip_address
    private_ip = azurerm_network_interface.node[m.name].private_ip_address
    ssh_user   = var.ssh_user
  }]
}

output "api_endpoint" {
  description = "host:port that will front the Kubernetes API for the user."
  value       = local.load_balancer ? "${azurerm_public_ip.api[0].ip_address}:6443" : "${azurerm_public_ip.node[local.control_planes[0]].ip_address}:6443"
}

# Optional in the contract: a module whose nodes reach the API another way than the user does.
output "api_node_endpoint" {
  description = "host:port the nodes reach the API through, when not api_endpoint: the internal load balancer of an HA cluster."
  value       = local.load_balancer ? "${azurerm_lb.api["api_nodes"].frontend_ip_configuration[0].private_ip_address}:6443" : ""
}

output "api_node_name" {
  description = "The name the nodes reach the API by, when a machine behind the internal load balancer can't reach it: each one resolves it in /etc/hosts (a worker to api_node_endpoint, a control plane to itself)."
  value       = local.load_balancer ? local.api_name : ""
}

output "storage_config" {
  description = "What the Azure Disk CSI driver's cloud config needs (IDs only): the tenant, the subscription, the resource group its disks go in, the location, and the managed identity the control planes carry."
  value = var.disk_csi ? {
    tenant_id      = data.azurerm_client_config.this.tenant_id
    subscription   = var.subscription
    resource_group = azurerm_resource_group.this.name
    location       = var.region
    identity       = azurerm_user_assigned_identity.disk_csi[0].client_id
  } : {}
}
