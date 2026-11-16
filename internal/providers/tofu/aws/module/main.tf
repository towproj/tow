provider "aws" {
  region = var.region

  # Every resource is findable by tag: delete uses it to prove nothing is left behind.
  default_tags {
    tags = local.tags
  }
}

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

  instance_types = toset([for n in values(local.nodes) : n.instance_type])
  arch           = { for t in local.instance_types : t => contains(data.aws_ec2_instance_type.node[t].supported_architectures, "arm64") ? "arm64" : "amd64" }

  # Only HA clusters need a load balancer in front of the API servers.
  load_balancer = length(local.control_planes) > 1

  # The operating systems var.os offers: who publishes the images, their names (by CPU
  # architecture, in the publisher's own words) and the login user they ship.
  images = {
    "ubuntu-24.04" = {
      owner = "099720109477" # Canonical
      name  = { amd64 = "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*", arm64 = "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-arm64-server-*" }
      user  = "ubuntu"
    }
    # AlmaLinux's own AMIs: free, no Marketplace subscription. The x86_64_v2 builds of
    # AlmaLinux 10 don't match: their names end in _v2.
    "almalinux-9" = {
      owner = "764336703387" # AlmaLinux OS Foundation
      name  = { amd64 = "AlmaLinux OS 9.* x86_64", arm64 = "AlmaLinux OS 9.* aarch64" }
      user  = "ec2-user"
    }
    "almalinux-10" = {
      owner = "764336703387"
      name  = { amd64 = "AlmaLinux OS 10.* x86_64", arm64 = "AlmaLinux OS 10.* aarch64" }
      user  = "ec2-user"
    }
  }
  image = local.images[var.os]

  user_data = templatefile("${path.module}/cloud-init.yaml.tftpl", {
    ssh_user              = var.ssh_user
    image_user            = local.image.user
    ssh_public_key        = var.ssh_public_key
    self_destruct_minutes = var.self_destruct_minutes
  })
}

data "aws_ec2_instance_type" "node" {
  for_each      = local.instance_types
  instance_type = each.value
}

# The newest image of the system, for each CPU architecture the machines need. Machines that
# run keep the image they were made from (ignore_changes below).
data "aws_ami" "image" {
  for_each    = toset(values(local.arch))
  most_recent = true
  owners      = [local.image.owner]

  filter {
    name   = "name"
    values = [local.image.name[each.value]]
  }
}

# Network: public subnets only. No NAT gateway, which would bill by the hour.

resource "aws_vpc" "this" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = { Name = var.cluster_name }
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id

  tags = { Name = var.cluster_name }
}

resource "aws_subnet" "public" {
  for_each                = toset(var.zones)
  vpc_id                  = aws_vpc.this.id
  availability_zone       = each.key
  cidr_block              = cidrsubnet(var.vpc_cidr, 8, index(var.zones, each.key))
  map_public_ip_on_launch = true

  tags = { Name = "${var.cluster_name}-${each.key}" }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.this.id
  }

  tags = { Name = var.cluster_name }
}

resource "aws_route_table_association" "public" {
  for_each       = aws_subnet.public
  subnet_id      = each.value.id
  route_table_id = aws_route_table.public.id
}

# Firewall: nodes talk freely to each other; from outside, only admin_cidrs reach SSH, the API
# and NodePort Services (as the nodes' own firewall rules allow).

resource "aws_security_group" "nodes" {
  name        = "${var.cluster_name}-nodes"
  description = "tow cluster ${var.cluster_name}"
  vpc_id      = aws_vpc.this.id

  tags = { Name = "${var.cluster_name}-nodes" }
}

resource "aws_vpc_security_group_ingress_rule" "intra_cluster" {
  security_group_id            = aws_security_group.nodes.id
  referenced_security_group_id = aws_security_group.nodes.id
  ip_protocol                  = "-1"
}

resource "aws_vpc_security_group_ingress_rule" "ssh" {
  for_each          = toset(var.admin_cidrs)
  security_group_id = aws_security_group.nodes.id
  cidr_ipv4         = each.value
  ip_protocol       = "tcp"
  from_port         = 22
  to_port           = 22
}

resource "aws_vpc_security_group_ingress_rule" "api" {
  for_each          = toset(var.admin_cidrs)
  security_group_id = aws_security_group.nodes.id
  cidr_ipv4         = each.value
  ip_protocol       = "tcp"
  from_port         = 6443
  to_port           = 6443
}

resource "aws_vpc_security_group_ingress_rule" "node_ports" {
  for_each          = { for pair in setproduct(var.admin_cidrs, ["tcp", "udp"]) : "${pair[0]}-${pair[1]}" => pair }
  security_group_id = aws_security_group.nodes.id
  cidr_ipv4         = each.value[0]
  ip_protocol       = each.value[1]
  from_port         = 30000
  to_port           = 32767
}

# The cluster's ingress (80, 443) and the file's access.ports, from the admin addresses.
resource "aws_vpc_security_group_ingress_rule" "open_ports" {
  for_each          = { for pair in setproduct(var.admin_cidrs, var.open_ports) : "${pair[0]}-${pair[1]}" => pair }
  security_group_id = aws_security_group.nodes.id
  cidr_ipv4         = each.value[0]
  ip_protocol       = "tcp"
  from_port         = each.value[1]
  to_port           = each.value[1]
}

# The API's load balancers reach the API servers from their private addresses inside the VPC:
# both health-check them, and the nodes' (internal) one forwards their API calls from there.
resource "aws_vpc_security_group_ingress_rule" "api_from_load_balancers" {
  count             = local.load_balancer ? 1 : 0
  security_group_id = aws_security_group.nodes.id
  cidr_ipv4         = var.vpc_cidr
  ip_protocol       = "tcp"
  from_port         = 6443
  to_port           = 6443
}

resource "aws_vpc_security_group_egress_rule" "all" {
  security_group_id = aws_security_group.nodes.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}

# The EBS CSI driver's permissions: AWS's own managed policy for it, on a role only the
# control planes carry (no keys anywhere).

resource "aws_iam_role" "ebs_csi" {
  count = var.ebs_csi ? 1 : 0
  name  = "tow-${var.cluster_name}-ebs-csi"
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = { Service = "ec2.amazonaws.com" }, Action = "sts:AssumeRole" }]
  })
}

resource "aws_iam_role_policy_attachment" "ebs_csi" {
  count      = var.ebs_csi ? 1 : 0
  role       = aws_iam_role.ebs_csi[0].name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonEBSCSIDriverPolicy"
}

resource "aws_iam_instance_profile" "ebs_csi" {
  count = var.ebs_csi ? 1 : 0
  name  = "tow-${var.cluster_name}-ebs-csi"
  role  = aws_iam_role.ebs_csi[0].name
}

# Machines

resource "aws_key_pair" "this" {
  key_name   = "tow-${var.cluster_name}"
  public_key = var.ssh_public_key
}

resource "aws_instance" "node" {
  for_each               = local.nodes
  ami                    = data.aws_ami.image[local.arch[each.value.instance_type]].id
  instance_type          = each.value.instance_type
  key_name               = aws_key_pair.this.key_name
  subnet_id              = aws_subnet.public[each.value.zone].id
  vpc_security_group_ids = [aws_security_group.nodes.id]
  user_data              = local.user_data

  # The EBS CSI driver's permissions, on the control planes only, where its controller runs.
  iam_instance_profile = var.ebs_csi && each.value.role == "control-plane" ? aws_iam_instance_profile.ebs_csi[0].name : null

  # A shutdown from inside (the test self-destruct) terminates the machine and its disk.
  instance_initiated_shutdown_behavior = var.self_destruct_minutes > 0 ? "terminate" : "stop"

  # IMDSv2 only: blocks the metadata-credential theft that IMDSv1 allows. The EBS CSI
  # driver's pods read the metadata (instance, zone) through one more network hop (its docs).
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = var.ebs_csi ? 2 : 1
  }

  # Burstable types default to "unlimited" credits, which bill beyond the hourly price.
  dynamic "credit_specification" {
    for_each = can(regex("^t[0-9]", each.value.instance_type)) ? [1] : []
    content {
      cpu_credits = "standard"
    }
  }

  root_block_device {
    volume_type = "gp3"
    volume_size = each.value.disk_gb
    encrypted   = true
    tags        = merge(local.tags, { Name = each.key })
  }

  tags = {
    Name       = each.key
    "tow-role" = each.value.role
  }

  lifecycle {
    # Both only matter when a machine is created. A newer Ubuntu image goes to new machines;
    # the ones running keep theirs. cloud-init reads user_data at first boot only, and a
    # change would make AWS stop and start the machine.
    ignore_changes = [ami, user_data]
  }

  # Machines need the internet route on first boot (cloud-init, package mirrors).
  depends_on = [aws_route_table_association.public]
}

# API load balancers (HA clusters only): two network load balancers in front of the control
# planes, one per kind of client.
#
#   - "api", internet-facing, for the user (kubectl): it keeps the caller's address, so the
#     nodes' security group above lets in admin_cidrs only, as with one control plane.
#   - "api_nodes", internal, for the nodes (kubelets, kube-proxy, the CNI, kubeadm join): its
#     address is private, so their API calls stay inside the VPC like the rest of their
#     traffic. It's the address kubeadm puts in front of the control planes. Client IP
#     preservation is off: with it on, AWS doesn't support a target reaching the load balancer
#     it's behind (hairpinning), and control planes do.
#     https://docs.aws.amazon.com/elasticloadbalancing/latest/network/edit-target-group-attributes.html#client-ip-preservation
#
# Both spread requests over every zone's API server (cross-zone), so a zone whose server is
# down drops nothing, and health-check every 10 s (kubeadm's docs: "a TCP check on the port
# the kube-apiserver listens on"), so a server that stops is out within ~20 s.

locals {
  api_load_balancers = local.load_balancer ? {
    api       = { name = "${substr(var.cluster_name, 0, 28)}-api", internal = false, preserve_client_ip = "true" }
    api_nodes = { name = "${substr(var.cluster_name, 0, 26)}-nodes", internal = true, preserve_client_ip = "false" }
  } : {}
}

resource "aws_lb" "api" {
  for_each           = local.api_load_balancers
  name               = each.value.name
  load_balancer_type = "network"
  internal           = each.value.internal
  subnets            = [for s in aws_subnet.public : s.id]

  enable_cross_zone_load_balancing = true
}

resource "aws_lb_target_group" "api" {
  for_each           = local.api_load_balancers
  name               = each.value.name
  port               = 6443
  protocol           = "TCP"
  vpc_id             = aws_vpc.this.id
  target_type        = "instance"
  preserve_client_ip = each.value.preserve_client_ip

  health_check {
    protocol            = "TCP"
    interval            = 10
    healthy_threshold   = 2
    unhealthy_threshold = 2
  }
}

resource "aws_lb_target_group_attachment" "api" {
  for_each = { for pair in setproduct(keys(local.api_load_balancers), local.control_planes) : "${pair[0]}-${pair[1]}" => {
    load_balancer = pair[0]
    node          = pair[1]
  } }
  target_group_arn = aws_lb_target_group.api[each.value.load_balancer].arn
  target_id        = aws_instance.node[each.value.node].id
  port             = 6443
}

resource "aws_lb_listener" "api" {
  for_each          = local.api_load_balancers
  load_balancer_arn = aws_lb.api[each.key].arn
  port              = 6443
  protocol          = "TCP"

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.api[each.key].arn
  }
}
