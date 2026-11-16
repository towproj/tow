# Contract shared by every cloud module; tow reads exactly these.

output "nodes" {
  description = "Every machine, in the order of var.machines: name, role, pool, public_ip (SSH), private_ip (node-to-node), ssh_user."
  value = [for m in var.machines : {
    name       = m.name
    role       = m.role
    pool       = m.pool
    public_ip  = google_compute_instance.node[m.name].network_interface[0].access_config[0].nat_ip
    private_ip = google_compute_instance.node[m.name].network_interface[0].network_ip
    ssh_user   = var.ssh_user
  }]
}

output "api_endpoint" {
  description = "host:port that will front the Kubernetes API for the user."
  value       = local.load_balancer ? "${google_compute_address.api["api"].address}:6443" : "${google_compute_instance.node[local.control_planes[0]].network_interface[0].access_config[0].nat_ip}:6443"
}

# Optional in the contract: a module whose nodes reach the API another way than the user does.
output "api_node_endpoint" {
  description = "host:port the nodes reach the API through, when not api_endpoint: the internal load balancer of an HA cluster."
  value       = local.load_balancer ? "${google_compute_address.api["api_nodes"].address}:6443" : ""
}

output "api_node_name" {
  description = "The name the nodes reach the API by, when a machine behind the internal load balancer can't reach it: each one resolves it in /etc/hosts (a worker to api_node_endpoint, a control plane to itself)."
  value       = local.load_balancer ? local.api_name : ""
}
