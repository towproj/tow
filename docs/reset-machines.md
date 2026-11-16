---
title: What tow changes on your own machines, and how it's undone
linkTitle: Your machines, undone
weight: 80
---
# What tow changes on your own machines, and how it's undone

On machines you list yourself (`provider: byoh`), tow records every change it makes in `/etc/tow/prepared.json` on each machine, readable by root only. Each entry says which cluster made the change, what it did, how to undo it (`undo`, for you), and the exact script tow runs to undo it (`revert`):

```bash
sudo cat /etc/tow/prepared.json
```

A machine's packages or settings that were there before tow are never recorded, so they're never undone.

## What `tow delete` does

| Command | Kubernetes | The preparation |
|---|---|---|
| `tow delete --name lab` | removed | stays; in a terminal, delete asks first (default: no) |
| `tow delete --name lab --reset` | removed | undone |
| `tow delete --name lab --yes` | removed | stays: undoing it unattended takes `--reset` |
| `tow delete --name lab --forget` | stays | stays: nothing is touched |

- **Before anything changes,** delete reads each machine's record and lists, machine by machine, what goes and what stays.
- **A machine it can't reach** stops it before anything changes. `--forget` is for a machine that's gone for good.
- **When an undo fails,** the cluster is kept. Run `delete` again: what was undone left the record and isn't done twice.
- **Volumes on the machines' disks** (local-path, OpenEBS, Longhorn) hold your data, so delete refuses a cluster that has some, and lists them. `delete -v` deletes them first, through Kubernetes, so each storage driver deletes its own data.

## What's recorded, and how each is undone

**Kubernetes** (removed by every delete but `--forget`):

| Change | Undone by |
|---|---|
| k3s | k3s's own uninstaller: `k3s-uninstall.sh` on control planes, `k3s-agent-uninstall.sh` on workers. It also removes k3s's network interfaces and rules, and its data, `/var/lib/rancher/k3s`. |
| kubeadm's init or join | `kubeadm reset -f`, then `/etc/cni/net.d` (kubeadm leaves it, its docs say) |
| kubelet, kubeadm, kubectl | purged with cri-tools and kubernetes-cni, then their apt repository and key (on RHEL-family machines: `dnf remove`, then `/etc/yum.repos.d/kubernetes.repo`) |
| containerd and runc | stopped, binaries and service removed, `/var/lib/containerd` deleted; a `config.toml` that was there before tow comes back |
| CRI-O | stopped, purged, its repository and settings removed |

**The preparation** (undone by `--reset`):

| Change | Undone by |
|---|---|
| packages (conntrack, open-iscsi…) | `apt-get remove` (on RHEL-family machines, `dnf remove`) of those packages only: no purge, no autoremove |
| kernel modules | tow's lines removed from `/etc/modules-load.d/k8s.conf`; the file goes when it's empty |
| kernel settings | tow's lines removed from `/etc/sysctl.d/k8s.conf`; the file goes when it's empty |
| swap | the lines tow commented out of `/etc/fstab` (`# tow: `) come back, swap units are unmasked, dphys-swapfile is enabled again, `swapon -a` |
| SELinux (kubeadm, RHEL family) | enforcing again, now (`setenforce 1`) and in `/etc/selinux/config`, as far as it enforced before |
| ufw rules | the rules commented `tow:<cluster>` are deleted. ufw stays on or off as it was: tow never turns it on. |
| firewalld | its zone `tow-nodes` and policy `tow-admin` are deleted (with `firewall-offline-cmd` when firewalld isn't running). firewalld stays on or off as it was. |
| NetworkManager (Calico) | `/etc/NetworkManager/conf.d/calico.conf` removed, NetworkManager reloaded |
| multipathd (Longhorn) | `/etc/multipath/conf.d/longhorn.conf` removed, multipathd restarted |

Each undone change leaves the record; once nothing is left in it, the file and `/etc/tow` go too.

## What stays until the machine restarts

- **Kernel modules and kernel settings:** they stay loaded and set; the next boot no longer applies them.
- **On kubeadm,** kube-proxy's iptables rules and the network's interfaces (`cni0`, `flannel.1`, `vxlan.calico`…). kubeadm's docs say the rules are harmless left as they are. k3s's uninstaller removes its own.
- **kube-vip's virtual IP,** if kube-vip was stopped before it could let the address go.

## Doing it by hand

For a machine tow can no longer reach, or after `--forget`, use the `undo` of each entry in the machine's record, newest first, Kubernetes first. As root:

```bash
# k3s
/usr/local/bin/k3s-uninstall.sh          # control plane
/usr/local/bin/k3s-agent-uninstall.sh    # worker

# kubeadm
kubeadm reset -f && rm -rf /etc/cni/net.d /etc/tow/kubeadm-done
apt-mark unhold kubelet kubeadm kubectl
apt-get purge kubelet kubeadm kubectl cri-tools kubernetes-cni
rm /etc/apt/sources.list.d/kubernetes.list /etc/apt/keyrings/kubernetes-apt-keyring.gpg
# (RHEL family instead: dnf remove kubelet kubeadm kubectl cri-tools kubernetes-cni && rm /etc/yum.repos.d/kubernetes.repo)
systemctl disable --now containerd
rm -f /usr/local/bin/containerd* /usr/local/bin/ctr /usr/local/sbin/runc /usr/local/lib/systemd/system/containerd.service
rm -rf /etc/containerd /var/lib/containerd && systemctl daemon-reload

# the preparation: see each entry's "undo"; for the firewall
ufw status numbered      # then 'ufw delete <number>' for each rule marked tow:<cluster>, highest number first
firewall-cmd --permanent --delete-zone=tow-nodes && firewall-cmd --permanent --delete-policy=tow-admin && firewall-cmd --reload

# last
rm -rf /etc/tow
```

Volume data folders are left for you to delete once you're sure you don't need them: `/opt/local-path-provisioner` (local-path on kubeadm), `/var/openebs/local` (OpenEBS), `/var/lib/longhorn` (Longhorn).
