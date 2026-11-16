# Contract shared by every cloud module: tow passes these same inputs to each one and
# reads the same outputs back (outputs.tf). Credentials are never inputs: the provider reads
# the API token from PROXMOX_VE_API_TOKEN.

variable "cluster_name" {
  type        = string
  description = "Cluster name; prefixes VM names and fills the tow-cluster tag."
}

variable "region" {
  type        = string
  description = "The Proxmox node (server) the VMs run on."
}

variable "machines" {
  type = list(object({
    name          = string # the VM's name, its key in OpenTofu's state
    role          = string # control-plane or worker
    pool          = string # "control-plane" or the worker pool's name
    instance_type = string # unused: Proxmox has no instance types
    cpus          = number
    memory_mb     = number
    disk_gb       = number
    zone          = string # unused: one Proxmox node
    address       = string # the VM's static IPv4 address
  }))
  description = "Every VM, control planes first, with its address. tow places each one once and keeps it there, so adding or removing VMs never moves the others."

  validation {
    condition     = alltrue([for m in var.machines : m.address != ""])
    error_message = "Every VM needs an address."
  }
}

variable "ssh_user" {
  type        = string
  description = "Login user on every VM, created by cloud-init with passwordless sudo."
}

variable "ssh_public_key" {
  type        = string
  description = "authorized_keys line installed for ssh_user."
}

variable "admin_cidrs" {
  type        = list(string)
  description = "Only these addresses may reach SSH and the Kubernetes API port (Proxmox firewall)."
}

variable "tags" {
  type        = map(string)
  default     = {}
  description = "Extra tags (e.g. tow-expires) added to every VM, as key.value."
}

variable "self_destruct_minutes" {
  type        = number
  default     = 0
  description = "Unused: a VM shutting itself down on Proxmox only stops, it isn't deleted. Test cleanup relies on delete and its leftover check."
}

# Proxmox-specific inputs.

variable "proxmox_url" {
  type        = string
  description = "Proxmox API address, e.g. https://192.168.1.10:8006."
}

variable "template_id" {
  type        = number
  description = "ID of the cloud-init template VM to clone (Ubuntu 24.04 cloud image)."
}

variable "storage" {
  type        = string
  description = "Storage for the VM disks and their cloud-init drives."
}

variable "bridge" {
  type        = string
  description = "Network bridge the VMs are plugged into."
}

variable "cpu_type" {
  type        = string
  default     = "x86-64-v2-AES"
  description = "The VMs' CPU model. RHEL 10-based templates (Rocky, AlmaLinux) need x86-64-v3 or host."
}

variable "prefix_length" {
  type        = number
  description = "Subnet prefix of the addresses, e.g. 24."
}

variable "gateway" {
  type = string
}

variable "dns_servers" {
  type = list(string)
}

variable "api_endpoint" {
  type        = string
  default     = ""
  description = "host:port of the API servers' virtual IP (kube-vip) when set; otherwise the first control plane."
}

variable "open_ports" {
  type        = list(number)
  default     = []
  description = "TCP ports open to admin_cidrs: the cluster's ingress (80, 443) and the file's access.ports."
}

variable "pod_cidr" {
  type        = string
  default     = ""
  description = "The cluster's pod range: pods' traffic between nodes that a network sends unwrapped (Calico with encapsulation none or *-cross-subnet) comes from it."
}

variable "service_lb_cidrs" {
  type        = list(string)
  default     = []
  description = "The addresses LoadBalancer Services get (the service load balancer's pool, as CIDRs): the admin addresses may reach them, any port."
}
