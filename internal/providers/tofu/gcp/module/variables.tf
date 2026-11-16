# Contract shared by every cloud module: tow passes these same inputs to each one and
# reads the same outputs back (outputs.tf). Credentials are never inputs; the provider finds
# them through Google's Application Default Credentials (gcloud auth application-default
# login, GOOGLE_APPLICATION_CREDENTIALS, a VM's service account).

variable "cluster_name" {
  type        = string
  description = "Cluster name; prefixes resource names and fills the tow-cluster label."
}

variable "region" {
  type = string
}

variable "machines" {
  type = list(object({
    name          = string # the machine's name, its key in OpenTofu's state
    role          = string # control-plane or worker
    pool          = string # "control-plane" or the worker pool's name
    instance_type = string # the machine type; "" = the module's default
    cpus          = number # unused on GCP: sized by machine type
    memory_mb     = number # unused on GCP
    disk_gb       = number # 0 = root_volume_gb
    zone          = string # the zone, one of the region's
    address       = string # unused on GCP: GCP assigns the addresses
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
  description = "Login user on every machine: Google's guest agent creates it from the SSH key in the metadata, with sudo."
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
  description = "Extra labels (e.g. tow-expires) added to every resource that takes labels."
}

variable "self_destruct_minutes" {
  type        = number
  default     = 0
  description = "Test runs only: Compute Engine deletes the machines this long after they start. 0 = never."
}

# GCP-specific inputs.

variable "zones" {
  type        = list(string)
  description = "The cluster's zones, chosen by tow when the cluster is created: new machines spread over them. Each machine's own zone is in machines."
}

variable "project" {
  type        = string
  description = "The project everything goes in."
}

variable "machine_type_archs" {
  type        = map(string)
  description = "Each machine type the machines use, with its CPU architecture (amd64 or arm64), as the Compute Engine API reports it: the image follows it."
}

variable "pd_csi" {
  type        = bool
  default     = false
  description = "The GCE PD CSI driver runs on the cluster: a service account the control planes carry, with the permissions to create and attach disks."
}

variable "subnet_cidr" {
  type        = string
  default     = "10.250.0.0/16"
  description = "The nodes' subnet; kept clear of the pod and service ranges the engines use by default."
}

variable "root_volume_gb" {
  type        = number
  default     = 20
  description = "Boot disk of machines whose pool sets none."
}

variable "default_instance_type" {
  type    = string
  default = "e2-medium"
}
