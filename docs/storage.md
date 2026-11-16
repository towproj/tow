---
title: Storage on the provider's own disks
linkTitle: Storage
weight: 50
---
# Storage on the provider's own disks

## AWS: EBS volumes (`storage.csi: [aws-ebs]`)

tow installs the AWS EBS CSI driver (1.68.0, chart 2.68.0) with a StorageClass `aws-ebs`: gp3, encrypted, created in the zone of the node its pod runs on.

- **Permissions:** the AWS module puts an IAM role with AWS's own `AmazonEBSCSIDriverPolicy` on the control planes, and only there. The driver's controller runs on them. No keys anywhere.
- **Instance metadata:** the driver's pods read it (the instance, its zone) through one more network hop. The machines allow two hops instead of one, as the driver's docs ask for clusters AWS doesn't manage.
- **Cleanup:** every volume is tagged with the cluster, so the check after `tow delete` finds any left behind.

## GCP: persistent disks (`storage.csi: [gcp-pd]`)

tow installs the GCE PD CSI driver (1.28.1) with a StorageClass `gcp-pd`: pd-balanced, created in the zone of the node its pod runs on. The driver ships no Helm chart; tow applies its release's own manifests (the `noauth` overlay, which uses the machine's service account instead of a key), with a few changes, each marked in [gcp-pd.yaml.tmpl](../internal/addons/gcp-pd.yaml.tmpl): no Windows parts, the controller on the control planes, the node plugin on every node (a tow cluster may be control planes only), and the cluster's label on every disk. A version in the file picks the driver's image; the manifests stay the tested release's.

- **Permissions:** the GCP module creates a service account that only the control planes run as, and the driver's controller runs there. No keys anywhere. The driver's own setup grants it `roles/editor` on the project; tow grants less: Compute Storage Admin (disks and snapshots), a custom role of the cluster's own to get machines and attach and detach disks (the driver's own custom role, plus reading operations), and Service Account User on that one account (attaching a disk to a machine that runs as an account asks for it).
- **Machine types:** pd-balanced works with the E2, N1, N2, N2D, C2 and T2 families; the newest ones (C3, C4, N4) take Hyperdisk only.
- **Cleanup:** every disk is labeled with the cluster, so the check after `tow delete` finds any left behind. A deleted custom role stays, marked deleted, for 7 days before Google purges it; a cluster made again under the same name takes it back.

## Azure: managed disks (`storage.csi: [azure-disk]`)

tow installs the Azure Disk CSI driver (chart 1.36.0) with a StorageClass `azure-disk`: StandardSSD, created in the zone of the node its pod runs on, in the cluster's resource group, tagged with the cluster.

- **Permissions:** the Azure module creates a managed identity that only the control planes carry, with Contributor on the cluster's resource group and nothing else. The driver's controller runs on the control planes and reads it from the instance metadata. No keys anywhere.
- **Its cloud config** is a Secret, `azure-cloud-provider` in `kube-system`, as the driver's docs set it up: the tenant, subscription, resource group, location and the identity's client ID. IDs only, nothing secret; tow takes them from OpenTofu once the machines exist.
- **Node names** are the VMs' names: the driver finds a node's VM by it.
- **Cleanup:** the driver's disks live in the cluster's resource group, which `tow delete` removes; Azure refuses to delete the group while a disk tow didn't make is still in it, so nothing goes unnoticed.

## Proxmox: disks as volumes (`storage.csi: [proxmox]`)

tow installs the Proxmox CSI plugin (0.22.0, chart 0.5.15) with a StorageClass `proxmox` on the file's storage (`spec.infrastructure.proxmox.storage`, `local-lvm` by default). A volume is a disk attached to the VM its pod runs on, and it moves with the pod to another VM on the same server. tow labels each node with its region (the cluster's name) and zone (the Proxmox node), as the plugin requires.

The plugin needs two things from you:

1. **Its own token**, with only its privileges. Never use the admin token tow creates VMs with: every pod that can read the plugin's Secret would hold it. On the Proxmox server, as root (from the plugin's docs):

   ```bash
   pveum role add CSI -privs "VM.Audit VM.Config.Disk Datastore.Allocate Datastore.AllocateSpace Datastore.Audit"
   pveum user add kubernetes-csi@pve
   pveum aclmod / -user kubernetes-csi@pve -role CSI
   pveum user token add kubernetes-csi@pve csi -privsep 0
   ```

   Export the result as `PROXMOX_CSI_TOKEN=kubernetes-csi@pve!csi=<secret>` before `tow apply`. tow reads it at run time, hides it from its output, and never writes it to the file or its state.
2. **Proxmox in a cluster.** One server can be a cluster of its own: `pvecm create <a name>`, as root. `apply` checks this before creating anything.

## Deleting a cluster that has such volumes

EBS volumes, persistent disks, Azure disks and Proxmox disks outlive the machines: a cloud volume keeps costing money, and a Proxmox disk keeps taking space. So `tow delete` refuses a cluster that has some, lists them, and changes nothing. `tow delete -v` deletes them with the cluster, after naming them in the confirmation. It does this through Kubernetes, before the machines go, so each driver deletes its own disks. Back up what you need first.
