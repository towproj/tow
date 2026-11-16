---
title: A running cluster's life
linkTitle: Lifecycle
weight: 40
---
# A running cluster's life

The cluster's file stays its source of truth: change it, run `tow apply`, and apply shows a plan (Infrastructure first, then Cluster), asks, and makes it. `--dry-run` only shows the plan; `--yes` doesn't ask. A change apply can't make is refused before anything changes, with the reason and the way to get there. A run that stops says what it had done and the command to run again; running it again plans the rest from what's really there.

## What can change, and what can't

| In the file | On a running cluster |
|---|---|
| machines: `count`, `machines`, pools added or removed | ✓ joined, or drained and removed, one at a time ([machines](#adding-and-removing-machines)) |
| labels, taints, `controlPlane.schedulable` | ✓ through the API |
| `infrastructure.access.adminCIDRs`, `access.ports` | ✓ firewalls follow (AWS, Proxmox, ufw) |
| `kubernetes.version` | ✓ an upgrade, alone in its apply ([below](#upgrading-kubernetes)) |
| component settings: `extraArgs`, `certSANs`, kubelet flags, `k3s` flags | ✓ one machine at a time ([settings](#changing-settings)); k3s's critical ones refused |
| `backups` | ✓ ([backups](backups.md)) |
| built-in add-ons on or off, their settings; `helmCharts`, `manifests` | ✓ at the version that runs; versions move with an upgrade |
| `kubeconfig` | ✓ renamed, merged or not |
| a machine's size (`instanceType`, `resources`, `disk`) | ✗ add a pool with the new size, apply, remove the old pool |
| `metadata.name`, `distribution`, `containerRuntime`, the provider and region | ✗ build a new cluster and move your workloads |
| networking: ranges, `clusterDomain`, `cni`, flannel's backend, kube-proxy replacement, Calico's encapsulation | ✗ build a new cluster and move your workloads |
| `apiEndpoint`, `apiLoadBalancer` | ✗ every node and certificate uses the API's address |
| how tow reaches the machines (`ssh`) | ✗ |
| containerd's or CRI-O's release | with an upgrade only |

A local (k3d) cluster takes only its own charts and manifests; anything else: delete it and apply again.

## Adding and removing machines

New machines are created (OpenTofu) or checked (your own), prepared, and joined; the plan names them. A machine leaving goes last, one at a time: drained (once the workloads with a pod on it have their other pods Ready, as for [upgrades](#how-it-goes); PodDisruptionBudgets respected, at most 10 minutes, `apply --drain-timeout`; nothing is forced), a control plane out of etcd while it still runs, Kubernetes removed, its Node deleted, then the machine destroyed (on your own machines, the cluster's firewall rules undone; all of tow's preparation too with `apply --reset`). The first control plane stays; an even number of control planes is warned about. A VM deleted outside tow is made again under its name and address.

## Upgrading Kubernetes

Raise `spec.kubernetes.version` in the cluster's file, then run `tow apply`. Nothing else upgrades a cluster: an empty or `stable` version never does. `apply` and `tow status` say when a newer patch of the release you run is out:

```
Upgrade: Kubernetes v1.37.4+k3s1 is out (v1.37.2+k3s1 runs): set spec.kubernetes.version: v1.37.4+k3s1 and apply to upgrade
```

On k3s, `v1.37.4` means k3s's first build of it (`v1.37.4+k3s1`); write `+k3s2` for a later build.

### What's refused, before anything changes

- **Skipping a minor release:** 1.36 to 1.38 is refused; upgrade to 1.37 first. Kubernetes and k3s upgrade one minor release at a time.
- **A downgrade:** Kubernetes has no supported way back to an older release.
- **A release that doesn't exist:** checked on dl.k8s.io (kubeadm) or k3s's releases before the plan is shown.
- **Other changes in the same apply:** an upgrade goes alone. Apply the file's other changes first, with the version that runs, then the new version. The versions of what tow installs (the CNI, the built-in add-ons, containerd, CRI-O) may change with it.
- **A container runtime that doesn't go with the new release:** containerd or CRI-O pinned to a release that doesn't support it; change its version in the same apply. One tow didn't install is never changed: the upgrade is refused.
- **An add-on moving too far:** Cilium and Longhorn move one minor release at a time, Calico from its two previous releases (their upgrade docs); never backwards. Pin the release in between with the upgrade.
- **The file going back while an upgrade is half done:** nodes already upgraded can't go back. Set the version they run, and apply to finish the upgrade.

### How it goes

`apply` shows the plan and asks first (`--yes` doesn't ask, `--dry-run` only shows it):

1. **An etcd snapshot**, copied to `~/.tow/backups/<cluster>/` like `tow backup` does ([backups](backups.md)). Its `.json` says `beforeUpgradeTo`: the release it was taken before.
2. **The nodes, one at a time:** control planes first, then workers, as k3s's and kubeadm's upgrade docs say. Each node is:
   - waited for, before it's touched: every workload with a pod on it must have its other pods Ready (so a second pod of theirs isn't taken away before the first came back), for at most the drain timeout;
   - drained: its pods move elsewhere, PodDisruptionBudgets respected, for at most 10 minutes (`apply --drain-timeout`); nothing is forced;
   - upgraded (below);
   - waited for: Ready at the new release;
   - after a control plane, checked: the API answers `/readyz` (etcd included) on every control plane; on kubeadm, its control-plane containers run and every etcd member is healthy too;
   - uncordoned.
3. **The CNI and the built-in add-ons move** to the releases this tow installs, or those the file pins, in install order: Cilium after its pre-flight check (which pulls the new images on every node), keeping the defaults of the release it was first installed at (`upgradeCompatibility`); Calico's CRDs before its operator. Your own `helmCharts` and manifests aren't touched.
4. **The new release is recorded** once every node runs it.

**k3s** upgrades a node with its own installer, at the new release, with the settings the node already has.

**kubeadm** follows its upgrade page, on each node:
1. the Kubernetes package repository moves to the new minor release, and the kubeadm package is upgraded;
2. `kubeadm upgrade apply` on the first control plane (after `kubeadm upgrade plan` checks it), `kubeadm upgrade node` on the others and on workers. kubeadm renews the certificates it manages, and upgrades CoreDNS (and kube-proxy, when the cluster has it) once the last control plane is done; tow spreads CoreDNS over the nodes again after;
3. the container runtime, when it doesn't go with the new release: containerd to the release tow installs (or the file's), CRI-O from its new minor release's repository;
4. kubelet and kubectl, held at the new release, and the kubelet restarted. The kubelet is never newer than the API server.

kubeadm keeps a copy of each control plane's manifests and etcd data from before the upgrade in `/etc/kubernetes/tmp`; delete it once the cluster runs well. kubeadm's docs also describe stopping the API server for a few seconds before an etcd upgrade, so requests finish first; tow doesn't.

A cluster of one machine stops its workloads while that machine is drained and upgraded. So does a cluster whose pods can run on one node only (one worker, control planes that take no pods): its pods wait while it's drained, and a PodDisruptionBudget that needs one of them running stops the drain. The plan says so in both cases; add a worker, or make the control planes schedulable, first.

**Databases and other replicated workloads:** before draining a node, tow waits until every workload with a pod there (a ReplicaSet, a StatefulSet, an operator's object) has its other pods Ready, as if each had a PodDisruptionBudget allowing one pod away at a time; your own budgets apply on top. That wait is only as good as the pods' readiness: a database replica on a local volume comes back on its own node and needs time to catch up, so its readiness should say "caught up", not just "accepts connections". With CloudNativePG, set the readiness probe to `type: streaming` with a `maximumLag` (`.spec.probes.readiness`): a replica is then Ready only once it streams from the primary within that lag, and the primary's node isn't drained before. Replication can stay asynchronous: the primary's orderly shutdown hands its last writes to a replica that streams. The lifecycle test does exactly this, with Postgres written to throughout the upgrade; before tow waited, the lab lost committed rows when the primary's worker was drained right after the replica's worker came back. A workload whose other pods never become Ready stops the upgrade after the drain timeout, named, with nothing changed on that node.

### When it stops partway

The error names the node and the step that failed, says which nodes are already upgraded, and ends with the command to run again. Running it again:

- **skips the nodes already upgraded**, read from each node's own release;
- **finishes a node it left cordoned:** waits for it, checks it, then uncordons it;
- **takes no second snapshot:** the first one is from before any node changed.

A drain that doesn't finish leaves the node cordoned and names the pods in the way (a PodDisruptionBudget that allows no disruption, a pod nothing manages). `tow status` shows which nodes run which release while an upgrade is half done.

### Going back

There's no downgrade. The snapshot taken before the upgrade holds the cluster's data from just before, and `tow restore` can put it back on the upgraded release ([below](#restoring-a-snapshot)): the data goes back, the release stays.

## Changing settings

Change a component's settings in the file, then run `tow apply`. The plan names each machine and what changes on it, and asks first.

What can change: the API server's, controller manager's, scheduler's and etcd's `extraArgs`, `apiServer.certSANs`, kubelet flags (cluster-wide, the control planes', each pool's), metrics-server on or off, and kube-vip announcing LoadBalancer Services. On k3s also `k3s.server` and `k3s.agent` `extraArgs`, the `backups` schedule, and k3s's bundled Traefik and local-path on or off.

- **One machine at a time, control planes first,** only those whose settings change.
  - **k3s:** the machine gets its new `config.yaml`, and k3s restarts with it.
  - **kubeadm**, as its reconfigure page says: the cluster's stored configuration is updated first (`kubeadm init phase upload-config`, so a later upgrade keeps it); each control plane's static pods are written again for the components whose settings changed (`kubeadm init phase control-plane …`, `init phase etcd local`), with a new API server certificate for new `certSANs` (the old one set aside), and kube-vip's static pod for its Services; each kubelet's flags are changed in `/var/lib/kubelet/kubeadm-flags.env` (kubeadm's own flags stay), its configuration pulled again when metrics-server goes on or off (serving certificates signed by the cluster), and the kubelet restarts. metrics-server and its certificate approver are then installed or removed.
- **No drain:** restarting k3s, a static pod or the kubelet keeps the machine's pods running.
- **The API stays up**, except for a few seconds while the control plane holding kube-vip's address restarts: kube-vip keeps the address there, and that machine's API server is starting again (about 10 seconds in the lab).
- **Checked before the next one:** the node is Ready, and after a control plane, the API answers `/readyz` on every control plane (on kubeadm, its components run and every etcd member is healthy too).
- **Turning a bundled add-on off** uses k3s's own `disable`, which removes it. local-path is refused while it still has volumes; the plan names the Ingresses Traefik serves.
- **A run that stops** names the machine and those already done; running it again skips them.

Refused, with the reason:

- **k3s's critical settings,** which k3s requires to be the same on every server (servers whose value differs fail to join), so they can't change one server at a time: `agent-token`, `cluster-cidr`, `cluster-dns`, `cluster-domain`, `disable-cloud-controller`, `disable-helm-controller`, `disable-network-policy`, `disable=servicelb` (servicelb on or off), `egress-selector-mode`, `embedded-registry`, `flannel-backend`, `flannel-external-ip`, `flannel-ipv6-masq`, `secrets-encryption`, `secrets-encryption-provider`, `service-cidr`; and `data-dir`, which holds each machine's data.
- **flannel's backend,** on either distribution: nodes on different backends can't reach each other.
- **containerd's and CRI-O's release:** they change with a Kubernetes upgrade.

## Certificates

Kubernetes' certificates, and the client certificate in the kubeconfig tow gave you, last a year (k3s's and kubeadm's defaults).

- **`tow status`** shows when the API server's and your kubeconfig's expire.
- **`tow apply` renews them** when either expires within 30 days; `apply --renew-certificates` renews them now. The plan says so first.
  - **k3s:** on each control plane in turn, k3s stops, `k3s certificate rotate` renews its certificates, and k3s starts and is checked. Then each worker's k3s restarts; it renews its own certificates within 120 days of their expiry.
  - **kubeadm:** on each control plane in turn, `kubeadm certs renew all`, then its control plane restarts (its static pods, which read their certificates when they start) and is checked. The kubelets renew their own certificates.
  - **Then your kubeconfig is fetched again,** with its renewed client certificate, and stored as before: in `~/.tow/clusters/<cluster>/` and, when it was, in your own kubeconfig.
- **An upgrade renews them too:** a cluster upgraded at least once a year never needs this.

## Restoring a snapshot

```bash
tow restore --name prod             # the newest backup on this machine
tow restore --name prod --snapshot prod-20261113T021206Z --dry-run
```

`restore` puts a running cluster's etcd back to one of its backups in `~/.tow/backups/<cluster>/` ([backups](backups.md)), **on the same machines**. Everything the cluster's API knew comes back as it was then: objects, Secrets, Helm releases. Every change since is lost. Volumes' data isn't in etcd and stays as it is.

- **Which snapshot:** the newest, unless `--snapshot` names one (`tow backup --name <cluster> --list` shows them).
- **Only these can be restored:** a snapshot of the release the cluster runs, or the one an upgrade took right before upgrading to it (the release stays the upgraded one). A newer release's snapshot, an older one, another cluster's, or a copy whose sha256 no longer matches is refused before anything changes. On kubeadm, etcd restores only snapshots of its own minor release: a pre-upgrade snapshot is refused when the upgrade moved etcd to another one.
- **A safety backup first**, of the cluster as it is now, so the restore itself can be undone; `--no-backup` skips it. When no control plane can take one (etcd is down, often the reason to restore), the restore goes on without it.
- **The plan** is shown and asked about first; `--yes` doesn't ask, `--dry-run` only shows it. The cluster is locked while it runs.

How it goes:

- **k3s** (its docs): k3s stops on every control plane; the first one is reset to the snapshot (`k3s server --cluster-reset --cluster-reset-restore-path`, with `--etcd-s3=false` when the file has k3s upload to S3) and starts; then each other control plane, one at a time, has its etcd data moved aside and joins the restored cluster. Workers keep running.
- **kubeadm** (Kubernetes' and etcd's docs): `etcdutl` is read from each control plane's own etcd, and the snapshot's etcd release checked against it, before anything stops. Then etcd and the API servers stop on every control plane (their static pods set aside; kube-vip's stays), every etcd member is restored from the same snapshot into its own data directory (with `--bump-revision` and `--mark-compacted`, so nothing watching the API sees revisions go backwards), and they start again. The control planes are checked (the API ready, every etcd member healthy), and the kubelet restarts on every node, as the docs advise.

After a restore:

- **The old etcd data stays**, moved aside (`/var/lib/etcd.before-restore-<time>`, or `<data-dir>/server/db.before-restore-<time>` on k3s), and the output names each one: delete them once the cluster runs well.
- **Nodes the snapshot has that the cluster no longer has** (machines removed since) come back NotReady; they're named, for you to delete with `kubectl delete node`. tow doesn't delete them.
- **Run `tow apply`** with the cluster's file: the snapshot holds the add-ons and charts as they were, and apply puts back what the file says.
- **A run that stops** says where, and the command to run again; running it again restores the same snapshot from the start.

Restoring onto new machines (when the old ones are gone for good) isn't possible yet.
