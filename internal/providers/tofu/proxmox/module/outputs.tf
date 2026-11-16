# Contract shared by every cloud module; tow reads exactly these.

output "nodes" {
  description = "Every machine, in the order of var.machines: name, role, pool, public_ip (SSH), private_ip (node-to-node), ssh_user. On a LAN both are the same address."
  value = [for m in var.machines : {
    name       = m.name
    role       = m.role
    pool       = m.pool
    public_ip  = m.address
    private_ip = m.address
    ssh_user   = var.ssh_user
  }]
}

output "api_endpoint" {
  description = "host:port that will front the Kubernetes API."
  value       = var.api_endpoint != "" ? var.api_endpoint : "${[for m in var.machines : m.address if m.role == "control-plane"][0]}:6443"
}
