<h1 align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="artwork/tow-horizontal-dark.svg">
    <img alt="tow" src="artwork/tow-horizontal-light.svg" width="300">
  </picture>
</h1>

> One file describes your Kubernetes cluster; tow builds it, keeps it as described, and takes it down.

Describe your cluster in one file: where the machines run (AWS, GCP, Azure, Proxmox VE, or your own), which Kubernetes (k3s or kubeadm), and which add-ons. tow builds exactly that. Change the file, run `apply` again, and the running cluster follows, one node at a time.

A kubeadm cluster built by tow passes Kubernetes' conformance tests (Sonobuoy, certified-conformance mode: 469/469) on AWS and GCP, after a full lifecycle including an upgrade.

🟦 **Status:** pre-release (v0.x). The file format, `towproj.github.io/v1alpha1`, may still change before v1.

## Install
From v0.1.0: apt, dnf, Homebrew, release binaries or `go install` ([docs/install.md](docs/install.md)). Until then, build it (Go 1.26 or newer):
```bash
git clone https://github.com/towproj/tow.git
cd tow
go build -o tow
./tow --help
```

## Quick start: a local cluster
Needs Docker and [k3d](https://k3d.io/#installation):
```bash
./tow apply -f examples/local.yaml --dry-run   # shows what it would do; creates nothing
./tow apply -f examples/local.yaml             # cluster "dev"; kubectl switches to context "dev"
./tow status --name dev                        # machines, nodes, and any pod that's stuck
./tow delete --name dev                        # removes the cluster and its kubeconfig context
```
Add `--verbose` to any command to see what tow runs (secrets are always hidden).

## A cluster on machines
```yaml
apiVersion: towproj.github.io/v1alpha1
kind: Cluster
metadata:
  name: lab
spec:
  flavor: homelab                  # optional preset: k3s, flannel, local-path, traefik, metrics-server
  infrastructure:
    provider: byoh                 # aws | gcp | azure | proxmox | byoh (your own machines) | local (k3d)
    access:
      ssh: {user: pi}
  controlPlane:
    machines: [{host: 192.168.1.10}]
  workers:
    - name: pis
      machines: [{host: 192.168.1.11}, {host: 192.168.1.12}]
```
```bash
./tow apply -f lab.yaml        # lists what it will change on your machines, asks, then builds the cluster
```
Every field, flavor and add-on: [docs/cluster-file.md](docs/cluster-file.md). Ready-made files for every platform: [examples/](examples/).

## Platforms

| Platform | Provider | Tested end to end |
|---|---|---|
| Your laptop (k3d) | `local` | yes |
| AWS | `aws` | one control plane, a whole lifecycle, EBS, conformance |
| GCP | `gcp` | one and three control planes, a whole lifecycle, persistent disks, conformance |
| Azure | `azure` | one control plane, managed disks |
| Proxmox VE | `proxmox` | one and three control planes, a whole lifecycle, every add-on |
| Your own machines | `byoh` | k3s and kubeadm, `delete --reset` |

Setup and details per platform: [docs/platforms.md](docs/platforms.md).

## What's in it
- **Kubernetes:** k3s or kubeadm (containerd or CRI-O), one or several control planes behind a cloud load balancer, your own, or kube-vip.
- **Network:** flannel, Calico (iptables or eBPF), Cilium (with or without kube-proxy, WireGuard).
- **Storage:** local-path, OpenEBS, Longhorn, NFS, SMB, AWS EBS, GCP persistent disks, Azure managed disks, Proxmox disks.
- **Load balancers and ingress:** MetalLB, kube-vip, Cilium; Traefik, Cilium Ingress and Gateway API.
- **Lifecycle:** changes to a running cluster, upgrades, etcd backups and restore, certificate renewal: [docs/lifecycle.md](docs/lifecycle.md), [docs/backups.md](docs/backups.md).
- **Safety:** secrets never in the file, every change on your machines recorded with its undo, volumes never deleted without `-v`.

## Limits
- **The file format** is `v1alpha1`: fields may still change before v1.
- **Not yet:** restoring onto new machines, backing up volumes (use Velero or your database's own backups), LoadBalancer Services from the clouds' own load balancers.
- **Systems:** Ubuntu and Debian, RHEL 9 and 10 and their rebuilds, x86-64. Nodes joined outside tow aren't known to `status` or `delete`.
- **Not run yet:** several control planes on AWS and Azure, and a whole lifecycle on Azure.

## Contributing
See [CONTRIBUTING.md](CONTRIBUTING.md); every test, by hand and automated: [docs/testing/README.md](docs/testing/README.md). Changes: [CHANGELOG.md](CHANGELOG.md).

## License
Apache 2.0: see [LICENSE](LICENSE) and [NOTICE](NOTICE). Created and maintained by **Anass Lasry**.
