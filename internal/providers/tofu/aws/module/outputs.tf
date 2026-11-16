# Contract shared by every cloud module; tow reads exactly these.

output "nodes" {
  description = "Every machine, in the order of var.machines: name, role, pool, public_ip (SSH), private_ip (node-to-node), ssh_user."
  value = [for m in var.machines : {
    name       = m.name
    role       = m.role
    pool       = m.pool
    public_ip  = aws_instance.node[m.name].public_ip
    private_ip = aws_instance.node[m.name].private_ip
    ssh_user   = var.ssh_user
  }]
}

output "api_endpoint" {
  description = "host:port that will front the Kubernetes API for the user."
  value       = local.load_balancer ? "${aws_lb.api["api"].dns_name}:6443" : "${aws_instance.node[local.control_planes[0]].public_ip}:6443"
}

# Optional in the contract: a module whose nodes reach the API another way than the user does.
output "api_node_endpoint" {
  description = "host:port the nodes reach the API through, when not api_endpoint: the internal load balancer of an HA cluster."
  value       = local.load_balancer ? "${aws_lb.api["api_nodes"].dns_name}:6443" : ""
}
