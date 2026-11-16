# Contract shared by every cloud module: tow passes these same inputs to each one and
# reads the same outputs back (outputs.tf). Credentials are never inputs; the provider finds
# them as Azure's tools do (az login, ARM_CLIENT_ID and its secret or OIDC token, a managed
# identity).

variable "cluster_name" {
  type        = string
  description = "Cluster name; names the resource group (tow-<name>) and fills the tow-cluster tag."
}

variable "region" {
  type        = string
  description = "The Azure location, like westeurope."
}

variable "machines" {
  type = list(object({
    name          = string # the machine's name, its key in OpenTofu's state
    role          = string # control-plane or worker
    pool          = string # "control-plane" or the worker pool's name
    instance_type = string # the VM size; "" = the module's default
    cpus          = number # unused on Azure: sized by VM size
    memory_mb     = number # unused on Azure
    disk_gb       = number # 0 = root_volume_gb
    zone          = string # the availability zone ("1", "2", "3"); "" in a location without zones
    address       = string # unused on Azure: Azure assigns the addresses
  }))
  description = "Every machine, control planes first, with where it goes. tow places each one once and keeps it there, so adding or removing machines never moves the others."
}

variable "os" {
  type        = string
  default     = "ubuntu-24.04"
  description = "The machines' operating system: ubuntu-24.04, almalinux-9 or almalinux-10."
  validation {
    condition     = contains(["ubuntu-24.04", "almalinux-9", "almalinux-10"], var.os)
    error_message = "os must be ubuntu-24.04, almalinux-9 or almalinux-10."
  }
}

variable "ssh_user" {
  type        = string
  description = "Login user on every machine: Azure creates it, with the key and passwordless sudo."
}

variable "ssh_public_key" {
  type        = string
  description = "authorized_keys line installed for ssh_user."
}

variable "admin_cidrs" {
  type        = list(string)
  description = "Only these addresses may reach SSH and the Kubernetes API port."
}

variable "open_ports" {
  type        = list(number)
  default     = []
  description = "TCP ports open to admin_cidrs: the cluster's ingress (80, 443) and the file's access.ports."
}

variable "tags" {
  type        = map(string)
  default     = {}
  description = "Extra tags (e.g. tow-expires) added to every resource."
}

variable "self_destruct_minutes" {
  type        = number
  default     = 0
  description = "Test runs only: the machines are deallocated (Azure's auto-shutdown) at the time the tow-expires tag says. 0 = never."
}

# Azure-specific inputs.

variable "zones" {
  type        = list(string)
  description = "The cluster's availability zones, chosen by tow when the cluster is created: new machines spread over them. [\"\"] in a location without zones."
}

variable "subscription" {
  type        = string
  description = "The subscription everything goes in."
}

variable "machine_type_archs" {
  type        = map(string)
  description = "Each VM size the machines use, with its CPU architecture (amd64 or arm64), as Azure reports it: the image follows it."
}

variable "disk_csi" {
  type        = bool
  default     = false
  description = "The Azure Disk CSI driver runs on the cluster: a managed identity the control planes carry, Contributor on the cluster's resource group."
}

variable "subnet_cidr" {
  type        = string
  default     = "10.250.0.0/16"
  description = "The VNet and its one subnet; kept clear of the pod and service ranges the engines use by default."
}

variable "root_volume_gb" {
  type        = number
  default     = 30
  description = "OS disk of machines whose pool sets none (Azure's Ubuntu and AlmaLinux images need 30 GB)."
}

variable "default_instance_type" {
  type    = string
  default = "Standard_B2s_v2"
}
