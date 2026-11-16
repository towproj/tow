---
title: The cluster file
weight: 20
---
# The cluster file

`tow.yaml` describes your whole cluster: the machines, where they come from, and the Kubernetes on them, down to the network, storage, load balancers and ingress. tow installs exactly what the file asks for, and nothing else. Every field has a default, and a **flavor** is an optional ready-made starting point: anything it sets can be overridden.

```yaml
apiVersion: towproj.github.io/v1alpha1
kind: Cluster
metadata:
  name: lab
spec:
  flavor: homelab                  # optional preset; fills only what this file leaves out
  kubernetes:
    distribution: k3s              # k3s | kubeadm | k3d (local only)
    version: v1.37.2               # left out: v1.37.2, the release tow tests; "stable": the newest
    apiEndpoint: 192.168.1.100:6443   # with several control planes (AWS creates a load balancer instead)
    networking:
      cni: cilium                  # flannel | calico | cilium | none
    storage: {csi: [longhorn], defaultClass: longhorn}
    serviceLoadBalancer: {provider: metallb, metallb: {addresses: [192.168.1.200-192.168.1.220]}}
    ingress: {controllers: [traefik]}
    metricsServer: {enabled: true}
    apiServer: {extraArgs: {audit-log-maxage: 30}}   # flags passed as written to the component
    addons:
      helmCharts: [{name: cert-manager, repo: https://charts.jetstack.io, chart: cert-manager, version: v1.19.1}]
  infrastructure:
    provider: byoh                 # aws | gcp | azure | proxmox | byoh (your own machines) | local (k3d)
    access:
      ssh: {user: pi}
  controlPlane:
    machines: [{host: 192.168.1.10}, {host: 192.168.1.11}, {host: 192.168.1.12}]
  workers:
    - name: pis
      labels: {disk: ssd}
      machines: [{host: 192.168.1.13}, {host: 192.168.1.14, ssh: {port: 2222}}]
```

- **Flavors:**

  | Flavor | Kubernetes | Network | Storage | Ingress | Service load balancer | metrics-server |
  |---|---|---|---|---|---|---|
  | `local` | k3d, as it ships | flannel | local-path | traefik | servicelb | ✓ |
  | `homelab` | k3s, as it ships | flannel | local-path | traefik | servicelb | ✓ |
  | `dev-test` | kubeadm | calico | local-path | traefik | none | ✓ |
  | `production` | kubeadm | cilium | longhorn | traefik | none | ✓ |

  Without a flavor you get Kubernetes with nothing optional, not even a network (CNI), except on local clusters, which get flannel unless you write `cni: none`. A cluster without a CNI works but its nodes stay NotReady; tow says so.
- **Machines:** `byoh` lists them under `machines`; `aws`, `gcp`, `azure`, `proxmox` and `local` create `count` of them. Sizes use `instanceType` on clouds (the cloud's own name for it: `t3.large`, `e2-standard-4`, `Standard_B2ms`) or `resources: {cpu, memory}` on Proxmox, plus `disk` (`40Gi`). Worker pools can differ in size, and each pool can have its own `labels`, `taints` and kubelet flags.
- **Machines are prepared as the official docs ask** (Kubernetes, k3s, and each component you pick): packages, kernel modules and settings, swap off for kubeadm, and the firewall's rules when ufw or firewalld is on. Each item is checked first and changed only if missing. On your own machines tow lists the changes and asks before making them (`apply --yes` skips the question); every change is recorded on the machine in `/etc/tow/prepared.json`, with how to undo it.
- **Systems:** Ubuntu and Debian (and their derivatives), prepared with apt, and RHEL 9 and 10 and their rebuilds (Rocky, AlmaLinux), prepared with dnf. tow reads each machine's `/etc/os-release`, so one cluster may mix them. On RHEL-family machines, k3s keeps SELinux enforcing (its installer adds k3s's policy, and tow turns it on in k3s); a custom k3s `data-dir` isn't supported under SELinux and is refused there. kubeadm sets SELinux to permissive, as its install page does (recorded, and enforcing again after `delete --reset`). On AWS, `spec.infrastructure.os` picks `ubuntu-24.04` (the default), `almalinux-9` or `almalinux-10` (AlmaLinux's own free images, login user `ec2-user`; `examples/homelab-aws-almalinux.yaml`; a kubeadm control plane there needs more than 2 GiB, since AlmaLinux keeps memory aside for kdump, and tow refuses a smaller one before creating anything: `examples/dev-test-aws-almalinux.yaml`). GCP offers the same three, from Canonical's and AlmaLinux's free images, with the login user `ubuntu` or `almalinux` (`examples/dev-test-gcp-almalinux.yaml`); so does Azure, from Canonical's images and AlmaLinux's Marketplace images (free, no terms to accept; `examples/disk-azure.yaml`). Proxmox VMs run their template's system (`examples/homelab-proxmox-rocky.yaml`).
- **firewalld**, when it runs, lets the cluster through with two objects of tow's own: the zone `tow-nodes` accepts the other nodes and the pod and Service ranges (k3s's docs use the trusted zone for those ranges; tow leaves yours alone), and the policy `tow-admin` lets the admin addresses reach the API, NodePorts and the cluster's open ports. A source firewalld already puts in another zone is refused, with the command that frees it. Both go with `delete --reset`.
- **Kubernetes release:** the one the file pins; left out, v1.37.2, the release tow is tested with; `stable` for the newest upstream release at apply time. A newer tow that tests a newer release doesn't count as a change to a cluster that left it out.
- **k3s goes on the machines** with its official installer, at that release (its first k3s build: v1.37.2+k3s1), and the installer checks the download's sha256 sum. Each machine's settings are written to `/etc/rancher/k3s/config.yaml`, readable by root only. The first control plane starts the cluster (embedded etcd, so it can grow to several control planes), the others join it one at a time, then the workers.
  - **Joining is checked both ways:** a joining machine verifies the cluster's certificate authority before it sends the join password, and workers get a password that can't join a control plane.
  - **Join passwords are never kept by tow;** they stay on the control planes.
  - **CoreDNS gets two replicas with several control planes**, on different machines (kubeadm runs two too). k3s ships one: when its machine goes away, Kubernetes waits five minutes before starting it elsewhere, and the cluster has no DNS meanwhile. k3s's manifest sets no replica count, so k3s keeps the two when it applies the manifest again.
  - **The kubeconfig** is kept in `~/.tow/clusters/<name>/kubeconfig` and added to yours as context `<name>`.
  - **apply finishes** only when the API answers from your machine and every node has joined (and is Ready when there's a network).
  - **An apply that stops partway** picks up where it stopped on the next run.
- **kubeadm builds the cluster** as the kubeadm docs describe: each machine gets a container runtime and the Kubernetes packages, then `kubeadm init` runs on the control plane and the workers join it.
  - **Several control planes**, as kubeadm's HA page sets them up: kubeadm init on the first uploads the certificates the others need, encrypted with a key tow makes (never saved; deleted from the cluster once they joined), then the others join one at a time, then the workers. The address in front of them (the cloud's load balancer, yours, or kube-vip's virtual IP) is the cluster's `controlPlaneEndpoint`, and the kubeconfig uses it. Your own load balancer is tested from the first control plane before init, as kubeadm's page does.
- **kube-vip** serves `apiEndpoint` on Proxmox and your own machines (the default there; `apiLoadBalancer: external` names your own load balancer instead): a virtual IP that one control plane holds and announces on the local network (ARP), moved to another by leader election when that one goes away. Only the API: Services get their addresses elsewhere. kube-vip v1.3.1, pinned by its image digest, set up as its docs say:
  - **kubeadm:** a static pod on each control plane, on the first before `kubeadm init` (reading `super-admin.conf` while init runs, then `admin.conf`), on the others once they joined.
  - **k3s:** a DaemonSet on the control planes, put in k3s's manifests folder before k3s starts. It reads the k3s admin kubeconfig on each control plane, not a service account as kube-vip's k3s page does: the service account reaches the API through the Service network, which a CNI that replaces kube-proxy only brings once the API answers on the virtual IP. One DaemonSet means one interface name: control planes whose interfaces are named differently are refused.
  - **The address** must be an IPv4 address on the control planes' network, port 6443; tow finds the interface that holds each control plane's address and refuses a virtual IP outside its network before anything changes.
  - **Each machine's kubeadm config** is written from the file (release, pod and service ranges, cluster domain, `certSANs`, each component's `extraArgs`, node labels, taints, kubelet flags) and checked with `kubeadm config validate` on every machine before anything starts. It goes to the machine in a file only root can read, removed right after.
  - **Joining is checked both ways:** each worker checks the cluster's certificate authority (by its hash) before it trusts the control plane. The join token is made by tow, lives an hour at most, never leaves the machines, and is deleted once the workers joined.
  - **kube-proxy** is left out when the CNI replaces it (Cilium's option, or Calico's eBPF).
  - **An apply that stops partway** picks up where it stopped: machines where kubeadm finished are left alone; one where it failed is reset (`kubeadm reset`) and tried again. A failure shows kubeadm's output, the kubelet's last log lines and the machine's containers.
  - **The kubeconfig** is kubeadm's admin one, kept and merged like k3s's.
  - **The runtime:** `containerRuntime: containerd` (the default) or `cri-o`. k3s brings its own containerd. Docker Engine (it needs cri-dockerd since Kubernetes 1.24), Podman (no CRI) and rkt (archived) are refused, with the reason.
  - **containerd:** a release from containerd's GitHub releases, 2.3.7 unless the file pins another (`containerd: {version: ...}`), with runc 1.5.2, each checked against a sha256 sum. Its `config.toml` comes from `containerd config default`, with systemd cgroups and the pause image kubeadm asks for. The release must be one containerd recommends for the Kubernetes minor (2.3 or 2.4 for 1.37); tow says what to pin otherwise.
  - **CRI-O:** from the CRI-O project's packages for the Kubernetes minor (download.opensuse.org, deb or rpm), the newest release unless the file pins one, with kubeadm's pause image in a drop-in.
  - **kubelet, kubeadm and kubectl** at the cluster's exact release, from pkgs.k8s.io, held there (`apt-mark hold`; on RHEL-family machines, the repository's `exclude` line, as kubeadm's install page writes it, with dnf), plus `crictl`. Each kubelet restarts every few seconds until kubeadm sets it up; that's expected.
  - **A runtime already on your machine** is used as it is when it goes with the Kubernetes release (for containerd: a recommended release, with its Kubernetes plugin on). Otherwise apply stops before changing anything and says why: tow never replaces or reconfigures a runtime it didn't install. The same goes for kubelet, kubeadm or kubectl of another release, k3s, or a machine already in a cluster tow didn't build.
- **The network (CNI):** k3s and k3d bring flannel. On machines, **Calico** and **Cilium** go on right after Kubernetes (k3s or kubeadm), from your machine, as their docs describe, at a tested release unless the file pins one. apply then waits until every node is Ready.
  - **flannel on kubeadm** (which brings no CNI) comes from flannel's own Helm chart, 0.28.10 unless the file pins one, with the cluster's pod range and the file's `backend` (vxlan by default, host-gw or wireguard).
  - **Calico:** VXLAN and the iptables dataplane by default; `encapsulation` and `dataplane: ebpf` are supported. See `examples/calico-proxmox.yaml` and `calico-ebpf-proxmox.yaml`.
  - **Cilium:** `kubeProxyReplacement` and WireGuard `encryption` are supported. See `examples/cilium-proxmox.yaml`.
  - **Without kube-proxy:** when the CNI replaces kube-proxy (Cilium's option, or Calico's eBPF), k3s runs without it, kubeadm skips it, and the CNI is given the API's real address.
  - **Tools:** Calico, and flannel on kubeadm, also need `kubectl` of the cluster's minor release (1.37 for a 1.37 cluster, any patch), which is checked and offered the same way as helm: tow offers the newest patch of that minor, the one dl.k8s.io names, checked against the sha256 dl.k8s.io publishes beside it.
- **Storage** (`storage.csi`, several at once; `examples/storage-proxmox.yaml`) is installed right after the network, each driver from its official source at a pinned release, waited for, and the file's `defaultClass` made the only default StorageClass:
  - **local-path** 0.0.37, from its release's manifest (k3s brings its own).
  - **OpenEBS LocalPV hostpath** 4.6.2, class `openebs-hostpath`. Its chart turns on five storage engines and a log stack by default; tow keeps hostpath only (LVM arrives with spare disks, in a later version).
  - **Longhorn** 1.13.0, `replicas` copies of each volume. Its machines get open-iscsi and nfs-common (iscsi-initiator-utils and nfs-utils on RHEL) and the iscsi_tcp module, and multipathd is kept off its volumes with Longhorn's own fix (sd* disks blacklisted in `/etc/multipath/conf.d/longhorn.conf`, recorded with its undo). A machine where multipath manages devices (a SAN) is refused, since that fix would cut their paths.
  - **NFS** (csi-driver-nfs 4.13.6) and **SMB** (csi-driver-smb 1.20.4): the driver, then a StorageClass `nfs` or `smb` for the file's share. SMB's user and password come from the environment variables the file names, into a Secret in kube-system.
  - **AWS EBS** (`aws-ebs`, `examples/ebs-aws.yaml`), **GCE persistent disks** (`gcp-pd`, `examples/pd-gcp.yaml`), **Azure managed disks** (`azure-disk`, `examples/disk-azure.yaml`) and **Proxmox disks** (`proxmox`, `examples/proxmox-csi.yaml`): each provider's own disks as volumes. The cloud drivers' permissions come from an IAM role, a service account or a managed identity on the control planes only; the Proxmox plugin takes a token of its own from `PROXMOX_CSI_TOKEN`. Setup and details: [storage.md](storage.md).
  - A version in the file is the chart's (or, for local-path and gcp-pd, the release's).
- **`tow delete` keeps your data safe:** EBS volumes, persistent disks, Azure disks and Proxmox disks outlive the machines, and on your own machines volumes keep their data on the machines' disks (local-path, OpenEBS, Longhorn), so a cluster that has some isn't deleted. delete lists them and changes nothing. `tow delete -v` deletes them with the cluster, through Kubernetes, after naming them.
- **A service load balancer** (`serviceLoadBalancer.provider`) gives LoadBalancer Services addresses from your pool (`addresses`: ranges, CIDRs or single addresses), announced on the local network (`mode: l2`, the default) or to your BGP routers (`mode: bgp`, with `bgp: {localASN, peers}`). Your machine (the admin addresses) may reach the pool's addresses, any port, through the Proxmox firewall and ufw; on Proxmox the pool must sit on the VMs' network. Layer 2 can't work on AWS, GCP or Azure (their networks ignore it), so it's refused there.
  - **MetalLB** 0.16.2 (`examples/metallb-proxmox.yaml`): its chart, its namespace allowed privileged pods as its docs ask, then an address pool and an L2 or BGP announcement.
  - **kube-vip** (`examples/kube-vip-lb-proxmox.yaml`): kube-vip-cloud-provider 0.0.13 hands out the pool's addresses. The kube-vip in front of the API announces them when there is one; otherwise a kube-vip DaemonSet of its own does, on each node's default-route interface. BGP isn't supported for it yet (kube-vip needs a router ID per node); use MetalLB or Cilium for BGP.
  - **Cilium** (`examples/cilium-lb-proxmox.yaml`): LB IPAM with L2 announcements or its BGP control plane, turned on with Cilium's own install. It works through Cilium's kube-proxy replacement, which must be on.
  - **servicelb** is k3s's own.
- **Ingress** (`ingress.controllers`): tow builds in only what needs it. **Traefik** (k3s's own, or its chart on kubeadm: on a pool address, or on every node's ports 80 and 443 without a load balancer) and **Cilium**'s Ingress and Gateway API (with Cilium's install; Gateway API's definitions go in first). Any other controller (F5 NGINX, HAProxy, Envoy Gateway, Contour…) is a chart or manifest under `addons`; [ingress.md](ingress.md) has tested entries to copy.
- **Firewalls:** ports 80 and 443 for ingress, and any TCP ports you list in `spec.infrastructure.access.ports` (for what you install yourself), are opened to the admin addresses in the AWS security group, the GCP firewall rules, the Azure network security group, the Proxmox firewall, ufw and firewalld. The admin addresses are your machine by default; `access.adminCIDRs` widens them, on your own machines too.
- **metrics-server** (chart 3.14.0, metrics-server 0.9.0) on kubeadm; k3s brings its own. kubeadm's kubelets then get serving certificates the cluster signs (`serverTLSBootstrap`), as kubeadm's certificate page says, so metrics-server can check them; kubelet-csr-approver 1.2.15 approves them, for the cluster's own node names and addresses only.
- **Manifests** (`addons.manifests`: https URLs, or files and folders next to the cluster file) are applied last with `kubectl apply --server-side`. Objects that name no namespace go into one named after the file (`app.yaml` → `app`), created by tow; write `{path: app.yaml, namespace: shop}` to pick it. Objects that name their own keep it.
- **etcd backups** (`backups: {schedule, keep}`, [backups.md](backups.md)): snapshots on each control plane on a schedule, k3s's own or a systemd timer on kubeadm. `tow backup --name <cluster>` takes one now and keeps a copy in `~/.tow/backups/<cluster>/`, checked against its sha256. The copies outlive the cluster. `tow restore --name <cluster>` puts one back, on the same machines ([lifecycle.md](lifecycle.md#restoring-a-snapshot)). A snapshot holds the cluster's Secrets: keep it safe.
- **Nothing goes into `default`:** a chart without a namespace gets one named after its release.
- **Helm charts** (`addons.helmCharts`) are installed once the cluster runs, in the file's order, with your own `helm` (3.8 or newer, or 4.x), from your machine (`examples/local-helm.yaml`). The file's values win over what a controller changed in a chart's objects, as Helm 3 always does: on Helm 4, which applies server-side, tow adds `--force-conflicts` to every `helm upgrade` it runs.
  - **Each chart** is installed with `helm upgrade --install --wait`, at the exact version the file pins.
  - **Values** go to helm in a file only you can read, deleted right after. A secret value comes from an environment variable (`valuesFromEnv`), never the file.
  - **A failed chart** keeps the cluster; the next apply installs the charts again.
  - **When helm is missing or too old**, tow says so and asks before installing the release it was tested with into `~/.local/bin`, its download checked against a sha256 built into tow. A helm you already have is never replaced without your yes. Unattended runs need `--install-tools`.
- **Pass-through settings** (`extraArgs`, chart `values`, manifests) go as written to the tool that uses them (kubeadm, k3s, Helm, the API server), and that tool's own error is shown if it rejects one. A flag a field of the file already sets is refused as a conflict.
- **Secrets never go in the file.** A flag or chart value that needs one names an environment variable instead (`etcd-s3-secret-key: {env: S3_SECRET_KEY}`, or `valuesFromEnv` for a chart). tow reads it when it runs and hides it from all its output; a key or password written in the file is refused.
- **Only real limits are enforced.** A field the provider doesn't use is refused rather than ignored, and every refusal says why; risky but working choices get a warning. Anything this version can't install yet is listed by name, never silently skipped.
- More examples in `examples/`: `custom-bare.yaml` is the smallest file, `custom-full.yaml` shows every field, and the other `custom-*.yaml` files use no flavor.
