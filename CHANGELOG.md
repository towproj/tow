# Changelog

All notable changes are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/): until v1, a minor release (v0.2.0) may change the cluster file format, and says how in this file.

## [Unreleased]

### Security
- When `apply` and `status` read the API server's certificate dates, the certificate is now checked as kubectl checks it (the kubeconfig's certificate authority, the server's name); only an expired one is still read, so `apply` can renew it. Before, it wasn't checked at all.

### Fixed
- Releases publish the Homebrew cask themselves again: v0.1.0's release stopped before it (a token setting GoReleaser refuses), and its cask's quarantine step was in the wrong syntax. v0.1.0's cask was written by hand and installs on Apple Silicon and Intel Macs.

## [0.1.0] - 2026-11-16
The first release.

### Machines
- Machines created with OpenTofu on AWS, GCP, Azure and Proxmox VE, or your own machines over SSH (`byoh`); k3d on your laptop (`local`).
- Machines spread over the cloud region's zones; several control planes behind two cloud load balancers (a public one for you, an internal one for the nodes), or kube-vip's virtual IP on Proxmox and your own machines.
- Ubuntu and Debian, and RHEL 9 and 10 with their rebuilds (Rocky Linux, AlmaLinux), prepared as the official docs ask: packages, kernel modules and settings, swap, SELinux, ufw or firewalld. Every change is recorded on the machine with its undo.

### Kubernetes
- k3s or kubeadm (containerd or CRI-O), at a pinned release or the newest stable one.
- Networks: flannel, Calico (iptables or eBPF), Cilium (kube-proxy replacement, WireGuard).
- Storage: local-path, OpenEBS, Longhorn, NFS, SMB, AWS EBS, GCE persistent disks, Azure managed disks, Proxmox disks.
- Service load balancers: MetalLB, kube-vip, Cilium (layer 2 or BGP); ingress: Traefik, Cilium Ingress and Gateway API; metrics-server.
- Your own Helm charts and manifests, with secrets from environment variables only.

### Lifecycle
- `apply` on a running cluster: machines added and removed, add-ons and settings changed, firewall rules, labels and taints, Kubernetes and CNI upgrades one node at a time.
- etcd backups on a schedule and on demand (`tow backup`), `tow restore`, certificates renewed before they expire.
- `tow status`, `tow delete` (volumes kept unless `-v`; `--reset` undoes the preparation on your own machines).

### Distribution
- `tow version` (and `--version`): the release, the commit it was built from, the Go version and platform.
- Releases for Linux and macOS (amd64 and arm64): archives, `.deb` and `.rpm` packages with shell completions, a Homebrew tap, checksums signed with cosign, SBOMs and build provenance.
- A documentation site (Hugo and Docsy) built from `docs/`, with search, on the project's GitHub Pages.
- Signed apt and yum repositories: `apt install tow`, `dnf install tow`, and later releases with `apt upgrade` or `dnf upgrade` ([docs/install.md](docs/install.md)).

### Tested
- A kubeadm cluster built by tow passes Kubernetes' conformance tests (Sonobuoy, certified-conformance mode: 469/469) on AWS and GCP, after a full lifecycle including an upgrade.
