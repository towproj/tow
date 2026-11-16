# Contract shared by every cloud module: tow passes these same inputs to each one and
# reads the same outputs back (outputs.tf). Credentials are never inputs; the provider
# finds them through the cloud's standard chain (profiles, SSO, env vars, instance roles).

variable "cluster_name" {
  type        = string
  description = "Cluster name; prefixes resource names and fills the tow-cluster tag."
}

variable "region" {
  type = string
}

variable "machines" {
  type = list(object({
    name          = string # the machine's name, its key in OpenTofu's state
    role          = string # control-plane or worker
    pool          = string # "control-plane" or the worker pool's name
    instance_type = string # "" = the module's default
    cpus          = number # unused on AWS: sized by instance type
    memory_mb     = number # unused on AWS
    disk_gb       = number # 0 = root_volume_gb
    zone          = string # the availability zone, one of zones
    address       = string # unused on AWS: AWS assigns the addresses
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
  description = "Login user on every machine; created by cloud-init when the image doesn't have it."
}

variable "ssh_public_key" {
  type        = string
  description = "authorized_keys line installed for ssh_user."
}

variable "admin_cidrs" {
  type        = list(string)
  description = "Only these addresses may reach SSH and the Kubernetes API port."
}

variable "ebs_csi" {
  type        = bool
  default     = false
  description = "The EBS CSI driver runs on the cluster: an IAM role with its policy on the control planes, and a metadata hop limit of 2."
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
  description = "Test runs only: machines delete themselves this long after boot. 0 = never."
}

# AWS-specific inputs.

variable "zones" {
  type        = list(string)
  description = "The cluster's availability zones, one public subnet each, chosen by tow when the cluster is created. A subnet's range comes from its zone's position here, so the list never changes order."
}

# AWS-specific knobs: optional, so the shared contract stays the same.

variable "vpc_cidr" {
  type        = string
  default     = "10.250.0.0/16"
  description = "Kept clear of the pod and service ranges the engines use by default."
}

variable "root_volume_gb" {
  type        = number
  default     = 20
  description = "Root disk of machines whose pool sets none."
}

variable "default_instance_type" {
  type    = string
  default = "t3.medium"
}
