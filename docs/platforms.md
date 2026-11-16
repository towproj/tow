---
title: Platforms
weight: 30
---
# Platforms

Where the machines come from: `spec.infrastructure.provider` in the [cluster file](cluster-file.md).

| Platform | Provider | Tested end to end |
|---|---|---|
| Your laptop (k3d) | `local` | yes |
| AWS | `aws` | one control plane, a cluster's whole life, EBS volumes, conformance (469/469); several control planes not yet (NLBs aren't in the free plan) |
| GCP | `gcp` | one and three control planes, a cluster's whole life, persistent disks, conformance (469/469) |
| Azure | `azure` | one control plane, managed disks; several control planes and a whole life not yet |
| Proxmox VE | `proxmox` | one and three control planes (kube-vip), a cluster's whole life, every network, storage and load balancer option |
| Your own machines | `byoh` | k3s and kubeadm on AWS servers (Ubuntu and AlmaLinux), including `delete --reset` |

Systems: Ubuntu and Debian and their derivatives, RHEL 9 and 10 and their rebuilds. Tested on Ubuntu 24.04, Rocky Linux 9 and 10, and AlmaLinux 9 and 10, x86-64; the clouds offer Ubuntu 24.04 and AlmaLinux 9 and 10 (`spec.infrastructure.os`).

## Servers on AWS
Requires [OpenTofu](https://opentofu.org) (`tofu` 1.12 or a newer 1.x; when it's missing, tow asks before installing the release it was tested with into `~/.local/bin`, sha256 checked) and AWS credentials from any standard source: `aws sso login` or `aws configure` on your machine, `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` or an OIDC role for service accounts and CI, or an instance role. Credentials never go in `tow.yaml`; tow refuses a file that contains them.
```bash
tow apply -f examples/dev-test-aws.yaml   # two t3.small servers; SSH, the API and NodePorts open to your IP only
tow status --name dev-aws
tow delete --name dev-aws                 # destroys everything and checks nothing is left
```
OpenTofu's files and state live in `~/.tow/clusters/<name>/tofu/`, never in your working directory. Its providers come from registry.opentofu.org, pinned to the releases tow is tested with, checked against their checksums, and downloaded once into `~/.tow/plugin-cache` for every cluster (AWS 6.72.0, Google 8.11.0, Azure 5.14.0, Proxmox 0.120.0); several applies can run at once. If an apply fails partway, fix the cause and run it again; servers already created are reused. If it's killed while OpenTofu runs, `tow delete` still finds what it created.

With several control planes (`examples/production-aws.yaml`), tow puts two network load balancers (NLBs) in front of them:
- **A public one, for you:** your kubeconfig points at it, and as with one control plane only your IP reaches the API.
- **An internal one, for the nodes:** kubeadm's `controlPlaneEndpoint`, which the kubelets, kube-proxy, the CNI and joins use. Its address is private, so the nodes' API calls stay on the VPC's private network like the rest of their traffic. It doesn't keep the caller's address, since AWS doesn't support a machine behind an NLB reaching that NLB otherwise, and control planes do.

The API certificate names both. NLBs aren't in AWS's free tier.

## Machines on GCP
Requires OpenTofu (as for AWS), a project with the Compute Engine and IAM APIs on (`gcloud services enable compute.googleapis.com iam.googleapis.com --project <id>`), and Google's Application Default Credentials: `gcloud auth application-default login` on your machine, `GOOGLE_APPLICATION_CREDENTIALS` (a key file, or a workload identity federation configuration) for service accounts and CI, or a VM's own service account. The file names the project and region: `gcp: {project: my-project-123456, region: europe-west1}`.
```bash
tow apply -f examples/dev-test-gcp.yaml   # two e2-medium machines; SSH, the API and NodePorts open to your IP only
tow delete --name dev-gcp                 # destroys everything and checks nothing is left
```
Each cluster gets its own VPC and subnet, firewall rules, and machines spread over up to three of the region's zones, each with a public address (no Cloud NAT). Google's guest agent creates the login user from tow's SSH key; the project's own SSH keys and OS Login stay out. With several control planes (`examples/homelab-gcp-ha.yaml`), tow puts two passthrough network load balancers in front of them, a public one for you and an internal one for the nodes, as on AWS. A machine behind a GCP load balancer can't reach it (Google routes the address back to the machine itself), so the nodes reach the API by a name, `api.<cluster>.internal`, which each machine resolves in its `/etc/hosts`: a worker to the internal load balancer, a control plane to itself (to the first control plane while it joins). The API certificate names it; `delete` takes the line away. A free trial allows 4 public addresses in all: one per machine, one per HA cluster's load balancer.

## VMs on Azure
Requires OpenTofu (as for AWS) and Azure credentials: `az login` on your machine, a service principal in `ARM_CLIENT_ID`, `ARM_TENANT_ID` and `ARM_CLIENT_SECRET` (as OpenTofu's azurerm provider reads them) or Azure's `AZURE_` variables and workload identity for CI, or a VM's managed identity. The file names the subscription and location: `azure: {subscription: <its ID>, location: swedencentral}` (new subscriptions may find a location closed to them, or a VM size not offered: `az vm list-skus --size Standard_B2s_v2 --query "[?length(restrictions)==\`0\`].locationInfo[].location"` lists where yours may use it).
```bash
tow apply -f examples/dev-test-azure.yaml   # two Standard_B2s_v2 VMs; SSH, the API and NodePorts open to your IP only
tow delete --name dev-azure                 # deletes the resource group and checks nothing is left
```
Everything of a cluster's goes in a resource group of its own, `tow-<cluster>`: a VNet and its subnet, a network security group, and VMs spread over up to three of the location's availability zones (none where the location has no zones), each with a static public address (no NAT gateway). Azure creates the login user from tow's SSH key, with no password. With several control planes (`examples/homelab-azure-ha.yaml`), tow puts two Standard load balancers in front of them, a public one for you and an internal one for the nodes; as on GCP, the nodes reach the API by a name each machine resolves in its `/etc/hosts`, since a machine behind an internal load balancer can't always reach it. OS disks are 30 GB at least: Azure's images are that big. Several control planes and a cluster's whole life are written but not run on Azure yet: they need more public addresses and vCPUs than a free subscription allows.

## VMs on Proxmox VE
Requires OpenTofu (as for AWS) and an API token in `PROXMOX_VE_API_TOKEN` (`user@realm!tokenid=secret`); set `PROXMOX_VE_INSECURE=true` only for a self-signed lab certificate. VMs are cloned from a cloud-init template and get fixed addresses from the range in the file. A template of RHEL 10 or one of its rebuilds needs `proxmox.cpuType: x86-64-v3` (or `host`): their kernel doesn't boot on Proxmox's default CPU model, `x86-64-v2-AES`.
```bash
tow apply -f examples/homelab-proxmox.yaml    # two VMs running k3s; kubectl switches to context "homelab-pve"
tow delete --name homelab-pve                 # removes the VMs and the context, and checks nothing is left
```
With several control planes, set `spec.kubernetes.apiEndpoint` to a free address on the VMs' network, outside their range: kube-vip holds it as the API's virtual IP, and the kubeconfig uses it (`examples/homelab-proxmox-ha.yaml`: three control planes, no workers). An existing load balancer of your own goes with your own machines (`provider: byoh`). Setting up a test server: [testing/proxmox.md](testing/proxmox.md).

## Your own machines
List them under `machines` with `provider: byoh` (see `examples/homelab.yaml`). tow checks them, asks before preparing them, and installs k3s or kubeadm. A machine where Kubernetes was installed by someone else, or for another cluster, is refused before anything changes. With ufw or firewalld on, the checks can't see past it before preparation opens the ports in it: tow warns instead, and if the install then stalls or fails with an unclear error, something else blocks those ports too (a cloud security group, another firewall).

Every change tow makes on a machine is recorded in `/etc/tow/prepared.json` there (root only), with how to undo it. `tow delete` undoes from that record:
- **Kubernetes goes:** k3s's own uninstaller, or `kubeadm reset` and what tow installed for it (the Kubernetes packages, containerd).
- **The preparation stays** (packages, firewall rules, swap, kernel settings) unless you pass `--reset`, or answer yes when delete asks. Only what tow recorded is undone, never what was there before; `--yes` alone keeps it.
- **A machine that's gone for good:** `tow delete --forget` forgets the cluster without touching any machine.

What stays until a restart, and how to undo it all by hand: [reset-machines.md](reset-machines.md).
