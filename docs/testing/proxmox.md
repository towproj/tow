---
title: Testing on Proxmox VE
linkTitle: Proxmox VE
weight: 10
---
# Testing on Proxmox VE

`make test-proxmox` runs `TestProxmoxInfra` against a real Proxmox VE server. The test:

1. Runs `tow apply` on a cluster file (by default [`examples/dev-test-proxmox.yaml`](../../examples/dev-test-proxmox.yaml), which creates one control plane and one worker) under a throwaway name like `test-pve-1762540000`.
2. Logs in to every VM over SSH and checks:
   - the OS is Ubuntu, cloud-init finished, and sudo works without a password
   - the hostname is right
   - the VM has at least 2 CPUs and about 1.7 GB of memory, the minimum kubeadm accepts
   - DNS, HTTPS and the Ubuntu package mirrors are reachable
   - every other node answers ping and TCP on its fixed address
3. Runs `tow delete`, which fails if Proxmox still lists any VM tagged for the cluster.

Delete runs even when a check fails, on Ctrl+C, or when the test hits its 20-minute deadline. A run costs nothing and takes a few minutes.

## Requirements

- A Proxmox VE 8 or 9 server that this machine can reach on port 8006, plus the VM network on port 22.
- About 5 GB of free memory on the server: two VMs of 2 GB each, plus Proxmox itself.
- An SSH key in `~/.ssh`. tow installs the public key on the VMs. OpenTofu (`tofu` 1.12 or a newer 1.x) is used if you have it; otherwise the test downloads the release tow was tested with into a throwaway folder.
- Ten free addresses on the VM network, outside any DHCP range, for the VMs' fixed IPs.

## Getting a Proxmox server

Pick whichever of these fits. The test only needs the API and a template.

- **A real server.** Install Proxmox from the [ISO](https://www.proxmox.com/downloads). The VMs join your LAN through the `vmbr0` bridge. Edit the `network` block of the example to match your LAN.
- **Hyper-V on Windows 11 Pro.** This setup is tested; the steps are below.
- **KVM on Linux.** Enable nested virtualization (`kvm_intel nested=1` or `kvm_amd nested=1`). Then run Proxmox as a libvirt VM on a NAT network with a fixed subnet and CPU mode `host-passthrough`. Not tested by us yet.
- **VirtualBox.** Turn on "Enable Nested VT-x/AMD-V" for the Proxmox VM, and use a NAT network with a fixed subnet. Not tested by us yet.

### Hyper-V lab

This lab matches the example file. Proxmox runs at `192.168.100.10` on a private network, `192.168.100.0/24`, with Windows as the gateway at `.1`. Run the PowerShell commands as Administrator.

1. **WSL, if tow runs there.** Put this in `C:\Users\<you>\.wslconfig`, then run `wsl --shutdown`:
   ```ini
   [wsl2]
   memory=4GB
   networkingMode=mirrored
   ```
   `mirrored` lets WSL reach the private network. The memory cap leaves room for Proxmox.
2. **Hyper-V and a private network with a fixed subnet.** Hyper-V's "Default Switch" changes subnet on every reboot, so create your own. Reboot after the first command:
   ```powershell
   Enable-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V -All
   New-VMSwitch -SwitchName tow-nat -SwitchType Internal
   New-NetIPAddress -IPAddress 192.168.100.1 -PrefixLength 24 -InterfaceAlias "vEthernet (tow-nat)"
   New-NetNat -Name tow-nat -InternalIPInterfaceAddressPrefix 192.168.100.0/24
   ```
3. **The Proxmox VM.** Use **Generation 1**: on Generation 2, the Proxmox 9.2 installer can't find its own ISO. Nested virtualization and MAC spoofing let the VMs inside Proxmox run and reach the network.
   ```powershell
   New-VM -Name proxmox -Generation 1 -MemoryStartupBytes 6GB -SwitchName tow-nat `
     -NewVHDPath C:\HyperV\proxmox.vhdx -NewVHDSizeBytes 80GB
   Set-VMMemory -VMName proxmox -DynamicMemoryEnabled $false
   Set-VMProcessor -VMName proxmox -Count 4 -ExposeVirtualizationExtensions $true
   Set-VMNetworkAdapter -VMName proxmox -MacAddressSpoofing On
   Set-VMDvdDrive -VMName proxmox -Path C:\ISO\proxmox-ve.iso
   Start-VM proxmox
   ```
4. **Install Proxmox.** In Hyper-V Manager, right-click **proxmox** → **Connect…**, then pick **Install Proxmox VE (Terminal UI)**. The graphical installer fails on Hyper-V's display. Use these values:
   - IP: `192.168.100.10/24`
   - Gateway: `192.168.100.1`
   - DNS: `1.1.1.1`
   - Hostname: `pve.lab.local`

   After the install, eject the ISO with `Set-VMDvdDrive -VMName proxmox -Path $null`.
5. **Updates.** Open https://192.168.100.10:8006 and log in as `root`. Then:
   1. Go to **pve → Updates → Repositories**, disable the two *enterprise* repositories, and add **No-Subscription**.
   2. Go to **Updates**, click **Refresh**, then **Upgrade**, and reboot.

Stop the VM when you're not testing (`Stop-VM proxmox`), because it holds its 6 GB of memory even when idle.

## The template

tow clones an Ubuntu 24.04 cloud image template with cloud-init. Create one in the Proxmox shell (**pve → >_ Shell**):

```bash
cd /root
wget https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img
qm create 9000 --name ubuntu-2404-template --memory 2048 --cores 2 \
  --net0 virtio,bridge=vmbr0 --scsihw virtio-scsi-pci
qm set 9000 --scsi0 local-lvm:0,import-from=/root/noble-server-cloudimg-amd64.img
qm set 9000 --ide2 local-lvm:cloudinit --boot order=scsi0 --serial0 socket --vga serial0
qm set 9000 --ciupgrade 0
qm template 9000
```

`--ciupgrade 0` skips the full package upgrade that Proxmox otherwise runs on every VM's first boot, which saves several minutes per VM. Packages are Phase 2's job. Only `root@pam` can change this setting, so an API token can't set it per VM; set it once on the template.

### A Rocky Linux template

For the RHEL-family tests, a template from Rocky Linux 10's cloud image (9001, `examples/homelab-proxmox-rocky.yaml`), and one from Rocky Linux 9's (9002, `examples/dev-test-proxmox-rocky.yaml`: the Rocky 9 image's URL is the same with 9 for 10, and it runs on the default CPU type, so leave out `--cpu`). Same steps, in the same shell:

```bash
cd /root
wget https://dl.rockylinux.org/pub/rocky/10/images/x86_64/Rocky-10-GenericCloud-Base.latest.x86_64.qcow2
qm create 9001 --name rocky-10-template --memory 2048 --cores 2 --cpu x86-64-v3 \
  --net0 virtio,bridge=vmbr0 --scsihw virtio-scsi-pci
qm set 9001 --scsi0 local-lvm:0,import-from=/root/Rocky-10-GenericCloud-Base.latest.x86_64.qcow2
qm set 9001 --ide2 local-lvm:cloudinit --boot order=scsi0 --serial0 socket --vga serial0
qm set 9001 --ciupgrade 0
qm template 9001
```

Rocky Linux 10, like RHEL 10, needs an x86-64-v3 CPU: on Proxmox's default model, `x86-64-v2-AES`, its kernel stops at boot and the VM never answers. The cluster file sets `proxmox.cpuType: x86-64-v3` for the VMs tow clones (the template's own setting isn't copied). The lab's Ryzen supports it through Hyper-V; on older hosts use `host`.

## The API token

tow and OpenTofu both read the token from `PROXMOX_VE_API_TOKEN`. Proxmox has no login command, and tow refuses any `tow.yaml` that contains a token.

**The simple choice for a lab** is a token with the `PVEAdmin` role on `/`:

```bash
pveum user add tow@pve
pveum aclmod / -user tow@pve -role PVEAdmin
pveum user token add tow@pve tow --privsep 0
```

**For anything else, use a role limited to what tow does.** These privileges cover cloning, sizing, cloud-init, firewall rules, power and status. They haven't been verified by a test run yet, so if something is refused, the error names the path:

```bash
pveum role add Tow -privs "VM.Allocate VM.Clone VM.Audit VM.PowerMgmt VM.Config.CPU VM.Config.Memory VM.Config.Disk VM.Config.Network VM.Config.Cloudinit VM.Config.Options VM.Config.HWType Datastore.AllocateSpace Datastore.Audit SDN.Use Sys.Audit"
pveum aclmod / -user tow@pve -role Tow
```

The token secret is shown **only once**. Keep it in a password manager, never in a file inside the repo.

## Running the test

```bash
export PROXMOX_VE_API_TOKEN='tow@pve!tow=<secret>'
export PROXMOX_VE_INSECURE=true      # only for a fresh install's self-signed certificate
make test-proxmox
```

Optional settings:

- `TOW_TEST_PROXMOX_CONFIG=path/to/file.yaml` tests a different cluster file. It must use `provider: proxmox`; the test replaces its name. For example:
  - `TOW_TEST_PROXMOX_CONFIG=../examples/homelab-proxmox.yaml` installs k3s as it ships on two VMs, then checks the cluster works from your machine: every node Ready, the bundled components running, and a hello world reached through a NodePort (see [cluster checks](README.md#cluster-checks)).
  - `calico-proxmox.yaml`, `calico-ebpf-proxmox.yaml` and `cilium-proxmox.yaml` do the same with Calico (iptables, then eBPF) and Cilium (kube-proxy replacement + WireGuard). They also check the CNI runs on every node and pods reach each other across nodes.
  - `TOW_TEST_PROXMOX_CONFIG=../examples/homelab-proxmox-ha.yaml` builds three control planes behind kube-vip (virtual IP 192.168.100.100, kubeadm + flannel, no workers), checks the cluster, then powers off the control plane holding the virtual IP: another must take it over while the API and a new hello world keep answering, and the VM must come back Ready. Add `TOW_TEST_KUBERNETES='{distribution: k3s}'` for the same with k3s. Three VMs of 2 GB: give the Hyper-V VM as much memory as Windows allows, and run one test at a time.
  - `TOW_TEST_PROXMOX_CONFIG=../examples/storage-proxmox.yaml` installs every storage driver (local-path, OpenEBS hostpath, Longhorn, NFS, SMB), metrics-server and a manifest on one control plane and two workers, then checks a volume of each class keeps its data from one pod to the next. The NFS and SMB shares are test servers the check starts in the cluster; the SMB credentials get throwaway values unless `SMB_USER` and `SMB_PASSWORD` are set.
  - `TOW_TEST_PROXMOX_CONFIG=../examples/homelab-proxmox-rocky.yaml` does the same as `homelab-proxmox.yaml` on Rocky Linux 10 VMs (template 9001, above): dnf, SELinux enforcing with k3s's policy, and the RHEL 10 kernel modules package.
  - `TOW_TEST_PROXMOX_CONFIG=../examples/dev-test-proxmox-rocky.yaml` is kubeadm with Calico on Rocky Linux 9 VMs (template 9002): pkgs.k8s.io's rpm repository, SELinux permissive. Add `TOW_TEST_KUBERNETES='{containerRuntime: cri-o}'` for CRI-O from its rpm repository.
  - `TOW_TEST_PROXMOX_CONFIG=../examples/custom-proxmox.yaml` tests a cluster without a flavor: kubeadm + flannel and two worker pools of different sizes (3 VMs of 2 GB; in the Hyper-V lab above this relies on idle VMs using far less than their size).
- `PROXMOX_VE_INSECURE=true` skips the certificate check, and tow warns every time it's set. To avoid it, give Proxmox a trusted certificate (**pve → System → Certificates**).

The VMs' firewall rules allow SSH, port 6443 and NodePort Services (30000-32767) only from this machine, and allow all traffic between the nodes. Proxmox enforces these rules only while the Datacenter firewall is on (**Datacenter → Firewall → Options → Firewall: Yes**), and tow warns when it's off. Proxmox keeps its own web UI and SSH reachable from its local network when you turn the firewall on.

## Cleanup and troubleshooting

- **A run was killed before delete.** Find VMs tagged `tow-cluster.test-pve-…` in the web UI. Each one also has an `tow-expires.<unix time>` tag. Remove them with `tow delete --name <name>`, or in the UI with **More → Remove**.
- **`can't reach the Proxmox API`.** Check that the server is running, then run `curl -sk -o /dev/null -w '%{http_code}\n' https://<server>:8006`, which should print `200`. From WSL, check that `networkingMode=mirrored` is set.
- **`certificate is not trusted`.** Set `PROXMOX_VE_INSECURE=true` for a lab, or install a trusted certificate.
- **`refused the API token (401)`.** The token is wrong or was deleted. Create a new one.
- **SSH to the VMs times out.** Check that the VM addresses are free and inside the bridge's network, and that the gateway routes them. If the Datacenter firewall is on, check that this machine's address is the one shown in the dry run: `tow apply -f <file> --dry-run`.
- **`these addresses are already in use`.** Before creating anything, tow knocks on ports 22 and 443 of every VM address, and of `spec.kubernetes.apiEndpoint` for a new cluster. Something answered, so pick another range. A machine whose firewall drops everything goes unnoticed, so keep the range outside your DHCP pool anyway.
- **VMs boot slowly.** Set `--ciupgrade 0` on the template (see above).
