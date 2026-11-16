---
title: Testing tow
linkTitle: Testing
weight: 90
---
# Testing tow

Every way to test tow, from the cheapest to the most expensive. Run the commands from the repository root, in Linux, macOS or WSL.

| Test | Command | Needs | Time | Cost |
|---|---|---|---|---|
| Unit tests | `make test` | Go | ~10 s | free |
| Local k3d clusters | `make test-integration` | Docker, k3d | ~6 min | free |
| A kubeadm cluster on AWS | `make test-aws` | AWS credentials | ~10 min | ~$0.03 |
| An HA cluster on AWS (opt-in) | `make test-aws-ha` | AWS credentials; two NLBs, outside the free tier | ~25 min | ~$0.07 |
| Your own machines (byoh) with k3s then kubeadm, each deleted with `--reset`, on AWS | `make test-byoh-aws` | AWS credentials | ~30 min | ~$0.05 |
| Proxmox VMs | `make test-proxmox` | a Proxmox server and token | ~6 min with k3s, more with kubeadm | free |
| A cluster's whole life on Proxmox | `make test-lifecycle DIST=k3s` (or `kubeadm`) | the same | about an hour, unattended | free |
| A kubeadm cluster's life on AWS | `make test-aws-lifecycle` | AWS credentials | ~45 min | ~$0.04 |
| A kubeadm cluster on GCP | `make test-gcp` | a GCP project ([gcp.md](gcp.md)) | ~15 min | ~$0.02 |
| An HA cluster on GCP (three control planes, no worker) | `make test-gcp-ha` | the same | ~30 min | ~$0.06 |
| A kubeadm cluster's life on GCP | `make test-gcp-lifecycle` | the same | ~45 min | ~$0.05 |
| A kubeadm cluster on Azure | `make test-azure` | an Azure subscription ([azure.md](azure.md)) | ~15 min | ~$0.03 |
| An HA cluster on Azure | `make test-azure-ha` | the same | ~30 min | ~$0.08 |
| A kubeadm cluster's life on Azure | `make test-azure-lifecycle` | the same | ~45 min | ~$0.07 |
| Kubernetes conformance on an upgraded cluster (opt-in) | `make test-lifecycle-conformance` | a Proxmox server and token | 3 to 4 h, unattended | free |
| Every Proxmox case, one at a time (the matrix) | `make test-proxmox-all` | the same | 5 to 6 h, unattended | free |
| Kubernetes conformance (opt-in, what CNCF's certification uses) | `make test-conformance` | the same, plus a cluster with two workers | ~2 h | free |
| Secrets scan | `make secrets` | gitleaks | seconds | free |

**When to run them:**
- `make test` runs before every commit. CI also runs it, with Go's race detector, together with the k3d test, the OpenTofu module checks and the secrets scan, on every push.
- The race detector needs a C compiler (`sudo apt install gcc`). On a machine with little memory, leave out `internal/core` and `internal/providers/tofu`: under `-race` they rebuild the AWS SDK, which takes about 3 GB. For example: `go test -race -p 1 $(go list ./internal/... | grep -v -e internal/core -e providers/tofu)`.
- The tests that create machines (AWS, GCP, Azure and Proxmox) use your `tofu` (1.12 or a newer 1.x) when you have one; otherwise they download the release tow was tested with (sha256 checked) into a throwaway folder. Tests never ask, so they never install anything into `~/.local/bin`.
- The AWS, GCP, Azure and Proxmox tests create real machines, so CI never runs them. Run them yourself when you change provisioning, byoh or `internal/remote`, and before merging a phase.
- Several can run at once (each from its own test binary, not two `make` targets in one folder, since they share `integration/integration.test`). A new AWS account allows 8 vCPUs of running instances, so two AWS tests at a time; ask for more under Service Quotas → EC2 → "Running On-Demand Standard instances".

**Every test that creates machines also removes them:**
- `delete` runs even after a failure, on Ctrl+C, or when the test hits its own deadline.
- `delete` then checks that nothing tagged for the cluster is left, and fails if something is.
- On AWS, GCP and Azure, the test then waits up to 5 minutes for the cloud to report every machine gone.

**Exit codes of the cloud targets (`make test-aws`, `test-aws-ha`, `test-aws-lifecycle`, `test-byoh-aws`, and the `test-gcp*` and `test-azure*` ones):**

| Code | Meaning |
|---|---|
| 0 | the checks passed and every machine is gone |
| 1 | a check failed; every machine is gone |
| 3 (make prints `Error 3`) | the cloud may still have machines from the run up, or `delete` failed, whether the checks passed or not. Not a test failure: the last lines name each machine and the command to shut it down (`aws ec2 terminate-instances ...`, `gcloud compute instances delete ...`, `az group delete ...`). They also go away by themselves within the hour (AWS: they shut down; GCP: Compute Engine deletes them, from their maximum run duration; Azure: auto-shutdown deallocates them, but their disks and addresses stay until the resource group is deleted). |

`go test` turns every failing exit code into 1, so these targets build the test binary and run it themselves.

If a test dies anyway, see [If a test dies midway](#if-a-test-dies-midway).

## Setup, once per clone

```bash
go version            # 1.26 or newer
tofu version          # optional: 1.12 or a newer 1.x (the tests that create machines download it otherwise)
k3d version           # local k3d test only, with Docker running
make hooks            # scan every commit for secrets (needs gitleaks)
ls ~/.ssh/id_ed25519.pub || ssh-keygen -t ed25519   # the clouds and Proxmox install this key on their machines
```

## 1. Unit tests

```bash
make test                                   # everything, no network or cloud needed
go test -short ./internal/config/...        # one package
go test -short -run TestProxmox ./...       # tests whose name matches
```

What they cover:

- **`internal/config`:**
  - every file in `examples/` parses, including the `custom-*` ones without a flavor
  - the order defaults apply in: the file, then the flavor, then the distribution
  - each flavor's components, and nothing optional without one (except flannel on local clusters)
  - a file with every field is accepted, without warnings
  - custom architectures are accepted (2 control planes with Cilium on k3s, kubeadm with flannel, several worker pools)
  - fields a provider doesn't use are refused, per provider
  - each validation error, for byoh, AWS, Proxmox and local, and the warnings: components per distribution, provider and CNI, options for a component that isn't selected, address pools that overlap anything, labels and taints, Helm charts and manifests
  - `extraArgs`: plain values, lists that repeat a flag, `{env: VARIABLE}`, and flags that conflict with a field or with what tow sets
  - credentials are refused in the file (by key in tow's own fields, by flag in `extraArgs`, Secret references allowed in chart `values`, and any value shaped like a secret anywhere)
  - the fingerprint used for drift detection: pools, machines, taints, charts and manifests keyed by name, sets in any order, lists whose order matters kept in order
  - what state keeps of each example file reads back as the same config
- **`internal/k3s`:**
  - the file becomes k3s's own flags: none for k3s as it ships, `--disable` for every bundled extra not asked for, per-pool labels, taints and kubelet flags, role labels left to the API
  - the install, against fake machines:
    - order: the first control plane, then the other control planes one by one, then the workers
    - each machine's `config.yaml`: the plain password on the first control plane, the `K10…` tokens with the CA's hash on joining machines, the same worker password on every control plane, `tls-san`, each pool's flags, YAML lists and quoting
    - the installer from the release's tag
    - the record in `/etc/tow/prepared.json`
    - a second run changes nothing, nor does a file whose labels or taints changed (k3s reads them only when a node registers)
    - k3s installed by someone else, for another cluster, or in another role is refused before anything changes
    - a failure shows the end of the k3s log
    - the user's own `{env}` tokens
    - the kubeconfig points at the API address
    - kube-vip: its DaemonSet goes on each control plane before k3s starts there, never on workers, with the interface holding the machine's address; the virtual IP is in `tls-san` and the kubeconfig; control planes whose interfaces are named differently, and a virtual IP off their network, are refused before anything changes
  - the dry run shows no secret, and its lines survive the output's redaction
  - the scripts parse with `sh -n`, and the read-only check runs in a local shell
- **`internal/kubeapi`:** waiting for nodes to join and be Ready, and role labels, against a fake API; labels and taints set and removed one by one, the others kept.
- **`internal/tools`:** finding helm against fake helms on PATH and in `~/.local/bin`:
  - supported releases are used as they are
  - a missing or unsupported helm is installed only after a yes, or with `--install-tools`; with nobody to ask, tow stops
  - the user's own helm stays as it was, and replacing the one in `~/.local/bin` names both releases
  - a download whose sha256 doesn't match installs nothing; an unknown platform is refused
  - a dry run installs nothing
  - `INTEGRATION=1` also downloads the real pinned helm and tofu into throwaway folders (part of `make test-integration`)
- **`internal/tools` (tofu):** 1.12 or a newer 1.x fits; the pinned release is one of them, with a sum for linux and darwin, amd64 and arm64 (OpenTofu's `SHA256SUMS`, its GPG signature checked when pinned); the download URL; `tofu version`'s output read.
- **`internal/addons` (CNIs):**
  - **Calico:** CRDs rendered by `helm template` and applied with `kubectl apply --server-side`, with the CRD API version that matches the cluster's release; the operator with the pod range as its IP pool; VXLAN without BGP by default, the other encapsulations with BGP; eBPF adds the API-address ConfigMap and the BPF dataplane.
  - **Cilium:** the pod range, kube-proxy replacement with the API address, WireGuard, and one operator replica on a one-node cluster.
  - **Both:** failures show the tool's error, and the dry run's commands.
- **`internal/tools` (kubectl):** only the cluster's minor release fits (any patch: one already on PATH is used); otherwise the newest patch of that minor, as dl.k8s.io names it, checked against the sha256 published beside it. A dry run fetches nothing; a named release of another minor, a sum that doesn't match, or a file that holds none, installs nothing. With `INTEGRATION=1`, real downloads of the newest 1.37 and 1.34.
- **`internal/addons`:** the helm command line for https and OCI charts; values and each secret in a file only the user can read, deleted afterwards; secrets never on the command line; the first failure stops the rest, with helm's own error; the dry run's commands.
- **`internal/addons` (running cluster):** the objects a manifest makes, read from kubectl's JSON (a list or a single object); a manifest is compared with what runs by `kubectl diff` and a server-side dry run, which change nothing (diff's exit 1 is a difference, 2 an error); applying it creates its namespace first and returns what it made; objects are deleted by name, one already gone being fine; Helm releases listed in every status with flags Helm 3 and 4 both take.
- **`internal/addons` (built-in add-ons on a running cluster):** each built-in add-on as a unit: what it is (storage, the default StorageClass, a service load balancer, an ingress controller), where its version is read (its Helm release, or its Deployment's image for those installed from a manifest), and what turning it off runs: Longhorn's deletion confirmation first, the objects it applied (never its namespace that way), its release or manifest, its own namespace (never kube-system), the Proxmox CSI plugin's node labels; the default class's annotation removed. A unit keeps the version that runs unless the file pins one. Two units are the same when they run the same commands with the same input; a StorageClass whose settings changed is made again; what a unit no longer applies is known (MetalLB from L2 to BGP drops its L2Advertisement). Cilium's settings compare the same way, its version kept. Versions read from `helm list` and image tags, and compared.
- **`internal/addons` (catalog):** every add-on of the first batch on kubeadm, in order (storage, the default StorageClass, kubelet-csr-approver then metrics-server, the charts, the manifests), each command checked: local-path's manifest at its tag and its rollout; OpenEBS with only hostpath on; Longhorn's replica count and no default class of its own; the NFS and SMB StorageClasses as their examples, the SMB credentials in a Secret and never in the dry run; kubelet-csr-approver allowing the nodes' names and addresses only; each manifest's namespace created, then the manifest applied with a kubeconfig copy whose context names it, a folder recursively; no values or kubeconfig copy left behind. On k3s, its bundled local-path and metrics-server install nothing. A failure names the add-on with the tool's output.
- **`internal/addons` (service load balancers):** MetalLB's privileged namespace, chart and objects (the pool, then an L2 announcement, or BGP peers in v1beta2 and a BGP announcement); kube-vip-cloud-provider at its tag with the pool as ranges, plus kube-vip's own DaemonSet only when no kube-vip serves the API; Cilium's pool (cidr and start/stop blocks) and L2 policy or BGP objects, and its values (L2 announcements or the BGP control plane). Every manifest decodes as YAML.
- **`internal/kubevip` (Services):** the API's kube-vip with Services on (its own Lease); the Services-only DaemonSet with its account and role, no interface given.
- **`internal/providers`:** a pool entry as the fewest CIDRs (ranges, CIDRs, single addresses, the address space's edges).
- **`internal/prepare` (pool):** ufw lets this machine reach the pool's addresses, as CIDRs.
- **`internal/addons` (ingress, provider storage):**
  - Traefik on kubeadm: on a pool address with a load balancer, or as a DaemonSet on every node's ports 80 and 443 without one; default class only when the file says so; k3s's own left alone.
  - Cilium's Ingress and Gateway values, with Gateway API's definitions (v1.6.3) applied before Cilium.
  - EBS: controller on the control planes, volumes tagged with the cluster, the gp3 class.
  - Proxmox CSI: nodes labeled with region and zone, privileged namespace, its token and class in the chart's values, the token never shown; a malformed token refused.
- **`internal/config` (ingress, ports):** only traefik and cilium in `ingress.controllers`; the others refused with a pointer to `addons.helmCharts`; Cilium's ingress needs its kube-proxy replacement and a load balancer; `access.ports` checked (range, duplicates; refused locally); byoh takes `adminCIDRs`.
- **`internal/prepare` (ports):** 80 and 443 for ingress and `access.ports` open to the admin addresses: this machine (`$ADMIN`), or each of the file's `adminCIDRs`.
- **`internal/providers` (Proxmox):** with the Proxmox CSI plugin, a server that isn't in a cluster is refused, with `pvecm create`; the module takes `open_ports`. **OpenTofu:** both modules validate with `open_ports`, the pool's CIDRs, and (AWS) the EBS role on control planes only and the metadata hop limit.
- **`internal/kubeapi` (volumes):** cloud volumes found by their driver; deleting them takes the pods using their claims first, then the claims, and waits until every volume is gone (one that stays is named).
- **`internal/kubeapi` (machines leaving):** a Node deleted (one already gone is fine), the local volumes tied to a node.
- **`internal/prepare` (machines leaving):** undoing Kubernetes and the cluster's firewall rules, the rest of the preparation kept.
- **`internal/providers/tofu` (machines):** the guard lets a run create and destroy exactly the machines it names (a destroyed one with its firewall and load balancer attachments), and create again a machine of the cluster's that's missing; nothing else; the machines a plan creates are named; a new machine's address loses any old host key.
- **`internal/kubeapi` (add-ons in use):** LoadBalancer Services, the Ingresses a class serves (and those naming none when it's the default), a Deployment's image (none when it's missing).
- **`internal/config` (backups):** cron schedules become systemd's `OnCalendar`, each checked with `systemd-analyze calendar` where it's installed (a weekday range from Sunday split, as systemd's week starts on Monday); refused for kubeadm's timer: a day of the month and a weekday both set, a step on a range or on weekdays; refused for both: a field out of range, a backwards range, a step of 0, a schedule without 5 fields; `keep` defaults to 5, from 1 to 100; none on a local cluster; k3s's own snapshot flags in `extraArgs` conflict with the field.
- **`internal/k3s` (backups):** the file's schedule and keep become `etcd-snapshot-schedule-cron` and `etcd-snapshot-retention`; without one, no snapshot flag.
- **`internal/kubeadm` (backups):** the snapshot timer goes on every control plane (never a worker), for containerd's or CRI-O's socket, checked by `sh -n`, recorded once in `prepared.json` with its undo; a new schedule is written over it without a second record; taking it off leaves the snapshots. The snapshot script runs in a real shell with a stand-in `crictl`: each run adds a root-only snapshot and keeps the newest `keep` (all of them with no schedule), nothing left in etcd's folder.
- **`internal/core` (backups):** `backup` takes a snapshot on the first control plane that answers, copies it to `~/.tow/backups/<cluster>/` (folder 0700, files 0600) with its `.json`, and lists it after the cluster's state is gone; a copy that doesn't match its sha256 isn't kept; no control plane answering names each one's error; a local cluster has none. Output sudo prints before the script is kept out of the copy. On a running cluster, a changed schedule is refused on k3s, and on kubeadm puts the timer on, rewrites it or takes it off, once per change.
- **`internal/versions` (upgrades):** the newest patch of a minor release, from dl.k8s.io's `stable-1.N.txt` and k3s's `v1.N` channel; whether a release exists (HEAD, 404 means no).
- **`internal/kubeapi` (upgrades):** each node's kubelet release, Ready and cordoned.
- **`internal/k3s` (upgrades):** a rolling install changes one machine at a time inside the caller's step (drain before, checks after), control planes first; a machine already on the release is skipped (a resumed upgrade); a failure stops the roll.
- **`internal/kubeadm` (upgrades):** a node goes as kubeadm's upgrade page says, against a fake machine: the kubeadm package (its repository moved to the new minor) and nothing else first, `kubeadm upgrade plan` then `apply --yes` on the first control plane, `kubeadm upgrade node` elsewhere, then containerd when it doesn't go with the new release, then kubelet and kubectl held and the kubelet restarted; run again, what's done is skipped (kubeadm upgrade runs again: it's idempotent); each machine's runtime change is named beforehand, and a runtime tow didn't install that doesn't go with the release stops it; the control-plane check (`/readyz` on the machine's own address, its containers, `etcdctl endpoint health --cluster`) passes `sh -n`.
- **`internal/addons` (upgrades):** Cilium moved to another release runs its pre-flight check (applied, its DaemonSet and Deployment waited for, deleted) before its chart, with `upgradeCompatibility` the release it was first installed at; Calico's CRDs are applied with `--force-conflicts`.
- **`internal/core` (upgrades):** an upgrade only when the file names a newer release (empty, `stable`, the same release in k3s's two spellings: none); refused: a downgrade, a skipped minor (naming those between), other changes in the same apply (the versions of what tow installs aside), a release that doesn't exist, an add-on moving backwards or further than its docs allow (naming the release to pin); kubeadm: the runtime changes in the plan, the first control plane with `upgrade apply`, CoreDNS spread again after; the CNI and built-in add-ons move to this tow's releases after the nodes, Cilium's first release recorded; the plan: a snapshot first (marked `beforeUpgradeTo`), then control planes, then workers, each drained, upgraded, Ready at the release, the API checked on every control plane after a control plane, uncordoned; a dry run changes nothing; a run that stops names the node and those already upgraded; the file going back while nodes run the newer release is refused; the next run finishes the node it left cordoned, takes no second snapshot, records the release; `apply` and `status` note a newer patch (silent when the channel doesn't answer); `status` shows nodes on different releases.
- **`internal/core` (settings):** k3s's new settings are planned per role and pool (flags added and removed, a secret named by its variable), then roll: each machine restarts k3s with them, a control plane checked before the next, nothing drained; refused: k3s's critical settings (and data-dir), on servers or agents; turning k3s's local-path off with volumes is refused, Traefik off names its Ingresses. kubeadm: the first control plane uploads the configuration and writes its changed static pods (a new certificate for new certSANs), each kubelet gets its new flags and configuration (metrics-server on: serving certificates), node by node, each checked; metrics-server and its CSR approver follow.
- **`internal/kubeadm` (settings):** a control plane's config for kubeadm's phases is init's without any token (kube-proxy left out as at init); the phases follow what changed, in order (upload, certificate set aside and made again, static pods), passing `sh -n`; kubeadm-flags.env keeps kubeadm's own flags while tow's change; kubelet flags carry secrets' values.
- **`internal/core` (certificates):** the API server's certificate is read from its TLS handshake and the kubeconfig's from its data; `status` shows both, and when one is near; within 30 days (or `--renew-certificates`) apply renews: k3s rotates on each control plane then restarts its workers, kubeadm renews on each control plane and restarts its static pods (the script passes `sh -n`), each checked; the kubeconfig is fetched again, keeping its context name and address; with an upgrade, only a note.
- **`internal/k3s` (restore):** k3s stops on every control plane, the first one is reset to the snapshot (its old reset-flag removed first, `--etcd-s3=false` when the file uploads to S3) and starts, then each other one moves its etcd data aside and joins, one at a time; one that can't stops it there; the scripts pass `sh -n`.
- **`internal/kubeadm` (restore):** etcdutl is read from each control plane's etcd and the snapshot's etcd release checked against it (another minor refused); then etcd and the API servers stop on every control plane before any member is restored, each with its own name and address, the whole cluster, a new token, `--bump-revision` and `--mark-compacted`, then they start again; kube-vip's static pod stays; the scripts pass `sh -n`.
- **`internal/core` (restore):** the newest backup by default, or the one named (with or without `.db`, or its path); only a snapshot of the running release or the pre-upgrade one, of this cluster and distribution; a dry run, or nobody to say yes, changes nothing; a safety backup first unless `--no-backup`; a copy whose sha256 changed is refused; k3s gets the file's data-dir and S3 setting; kubeadm: the snapshot to every control plane, checked, restored, the control planes checked, every kubelet restarted, cleaned up; two backups in the same second never share a file.
- **`internal/core` (delete -v):** a cluster with cloud volumes isn't deleted without `-v`, the volumes named and nothing changed (also when they can't be listed); with `-v` they're named in the confirmation and deleted before the machines. On your own machines, local-path, OpenEBS and Longhorn volumes count too.
- **`internal/prepare` (undo):** each revert script runs in a real shell on a temporary `/etc`, with stand-ins for ufw, apt and systemctl: swap's `/etc/fstab` lines come back, only tow's lines leave `k8s.conf` (the file goes once empty), only the cluster's ufw rules go (not `lab2`'s, IPv6 ones too), only packages still installed are removed. Undo removes Kubernetes first, then the preparation when asked, newest first; what's undone leaves the record even after a failure, so a second run picks up where it stopped; entries without a script and other clusters' stay; an empty record is removed. A record that can't be read, or a machine that can't be reached, is an error naming it.
- **`internal/core` (delete on your machines):** Kubernetes removed and the preparation kept, both listed machine by machine with what must be undone by hand; `--reset`; the question asked only without `--reset` or `--yes`, and only when there's preparation to undo; a machine that can't be read stops everything before any change, a failed undo keeps the state, both pointing to `--forget`; `--forget` touches no machine; `--reset` and `--forget` refused on clusters tow created the machines of.
- **`internal/core` and `internal/kubeapi` (status):** healthy only when the machines run, the API answers, every node is Ready and no pod is stuck; a node down, a stuck pod, an API that doesn't answer are each shown; a long list of stuck pods is cut. Stuck means crashing, an image that won't pull, unschedulable, failed; a failed pod a controller replaced, or of a Job that succeeded since, isn't.
- **`internal/prepare` (Longhorn):** multipathd kept off Longhorn's volumes with Longhorn's blacklist, written once, with its undo; refused where multipath manages devices.
- **`internal/kubecfg`:** a kubeconfig is renamed to the file's context, merged into `$KUBECONFIG` next to other clusters, and removed again; files are owner-only; a context added without moving the user off their current one, unless it's the same cluster's old name.
- **`internal/state`:** a second run on a cluster stops at once (the lock), another cluster isn't held up, and a lock alone isn't a cluster.
- **`internal/secretenv`:** secret variables never reach the programs tow starts; a value read is hidden from all output.
- **`internal/providers/k3d`:** the k3d command for each setting; a dry run shows `$VARIABLE`, never a secret.
- **`internal/core`:**
  - apply, status and delete with fake providers
  - a failed cluster is retried
  - a failed cluster whose machines may run Kubernetes keeps them (the guard), even after a refused run
  - every field of the file has a change class (a walk over the config's types), and every class names a real field
  - a running cluster: changes it can't follow are refused first, with the reason (a new or renamed pool counts once, a local cluster refuses all but its own charts and manifests); the plan shows the file's changes, then infrastructure, then cluster
  - the file's own Helm charts on a running cluster: one added, changed, or whose release is gone is installed (`helm upgrade --install`), one that left the file uninstalled after everything else, an unchanged one left alone; a release moving namespace is refused
  - machines added and removed on a running cluster: OpenTofu's first run names the new ones (and keeps the leaving ones), every machine is prepared for them, then k3s or kubeadm joins them and they're waited for; the leaving ones go last, one at a time: drained (with `--drain-timeout`), a kubeadm control plane out of etcd, Kubernetes removed, the Node deleted, then OpenTofu's second run names them; on the user's own machines only the new ones are checked, and one leaving keeps its preparation but not the cluster's firewall rules (all of it with `--reset`); state follows each machine; a drain that doesn't finish stops the run with the machine kept
  - a machine deleted outside tow is made again under its name: its Node (and a kubeadm control plane's etcd member) cleared first, then it joins; never the first control plane; refused before anything changes: the first control plane leaving, a machine changing role or pool, control planes added where the API's address would change (kubeadm without an address in front, AWS's single control plane)
  - the built-in add-ons on a running cluster, compared at the version that runs: a load balancer the file replaces goes first, Cilium's new settings next, then what's turned on or changed in install order (a StorageClass whose settings changed made again), the file's own charts and manifests, and what's turned off last; an unchanged file changes nothing; a changed add-on keeps the version that runs, one whose release is gone is installed again; refused before anything changes: another version of one that runs, storage that still has volumes, k3s's bundled add-ons, kube-vip's Services while it serves the API, Calico's encapsulation; the plan names the LoadBalancer Services and Ingresses a removal affects; a dry run changes nothing; `apply` and `status` name the add-ons this tow installs a newer release of, unless the file pins theirs
  - the file's own manifests on a running cluster: each compared with what runs and applied again when it differs; the objects it dropped, and those of a manifest that left the file, deleted before any chart is uninstalled; state keeps what each one made; one whose namespace was deleted by hand is applied again, and only what it no longer has is deleted, never what it just made; its objects moving namespace is refused, writing the default namespace out isn't a move
  - labels and taints follow the file through the API, only those the file has or had, the control-plane taint with `schedulable`; a node tow didn't make is reported, never touched
  - a new kubeconfig context renames the cluster's kubeconfig here and in yours
  - an unchanged cluster's infrastructure is planned again: up to date, or put back after a yes (`--yes`, never in a dry run)
  - every apply checks the machines as preparation does; what they need is planned and made under the plan's yes, preparation not asking again
  - when this machine can't reach the machines while OpenTofu has changes (its address moved), they're planned and changed right after OpenTofu, in the same run; with nothing for OpenTofu to do, that's an error
  - the tofu a provider runs is found, or installed after a yes, before its PreFlight: when apply creates a cluster, checks a running one, and when delete destroys one; `--install-tools` installs without asking, a dry run only says what it would install, and no tofu stops everything before PreFlight
  - a run that stops says what it had done, and doesn't record the file; two runs on one cluster: the second stops at once
  - status names nodes tow didn't make, and machines that aren't nodes
  - mode switches are refused
  - a machine can only be in one cluster
  - `--dry-run` creates nothing
  - what this version doesn't install yet is listed by name
- **`internal/providers/tofu`:**
  - every target's module has the shared inputs and outputs, and a lock file from registry.opentofu.org with checksums for linux and darwin, amd64 and arm64
  - without a tofu tow works with, PreFlight stops before any cloud call and says how to get one
  - module files are written with mode 600; every machine reaches the module with every field, named as always, control planes first
  - the plan summary
  - the guard: a replaced or deleted resource (a tainted machine, an attribute that can't change in place, anything no longer asked for) is refused once Kubernetes may run on the machines, and a missing machine on a running cluster
  - apply runs the plan the guard checked, and nothing after a refusal
  - machines keep their place from the last inputs; a running cluster keeps its test expiry, and this machine's address is detected every time: when it moved, the plan says so first
  - the guard lets a run that changes the admin addresses or the ports replace or delete the admin firewall rules, and nothing else (never the rule between nodes, never a machine)
  - delete rechecks leftovers
  - the target checks stop apply before OpenTofu runs
  - only a target's own package may import its cloud SDK
- **`internal/providers/tofu/aws`:**
  - a new cluster is spread round robin over its zones (every shape of up to three control planes and two pools)
  - adding machines moves none of the others; a new one goes where its pool has the fewest
  - instance types: only zones that offer them, a misspelled one, types with no zone in common
  - a cluster whose machines all have a zone asks AWS nothing
- **`internal/providers/tofu/proxmox`:**
  - against a fake Proxmox API: token problems, an unknown node, a missing template, a self-signed certificate
  - only the cluster's own VMs count for status and leftovers
  - a new cluster gets the range in order; later a VM keeps its address, and a new one gets the lowest free
  - addresses already in use are refused
- **`internal/providers/byoh`:** the checks each machine must pass (Ubuntu or Debian, or RHEL 9 or 10 and their rebuilds; CPU, memory, sudo, ports), run against local scripts; a port ufw or firewalld blocks before preparation is a warning naming it.
- **`internal/prepare` (RHEL family):** a system's family from `/etc/os-release` (RHEL, Rocky, AlmaLinux, any that says it's like RHEL; 8 and older, Fedora and machines without the file refused); packages by their name on each family, asked of dpkg or rpm, installed with dnf, and removed by their record against stand-ins for rpm and dnf; kernel-modules-extra for k3s on RHEL 10, built for the running kernel.
- **`internal/prepare` (firewalld):** with firewalld on, the zone `tow-nodes` (target ACCEPT) takes the other nodes and the pod and Service ranges, and the policy `tow-admin` (from any zone to the machine) the admin rules as rich rules, `$ADMIN` filled in on the machine; once there, nothing more; a source that left and an old admin address are removed, and no admin rule when this machine's address isn't known; refused: a source firewalld already has in another zone, firewalld older than 0.9; Calico warns. Rich rules are compared as firewalld writes them (a single address with or without /32). The scripts run against a stand-in firewall-cmd (and firewall-offline-cmd for the undo with firewalld stopped), and the facts script reads a stand-in firewalld.
- **`internal/kubeadm` (RHEL family):** against fake Rocky machines: pkgs.k8s.io's rpm repository written as kubeadm's install page writes it (its exclude line holding the packages), dnf installing the release with the exclude lifted (and without weak dependencies on RHEL 10), containerd as on Ubuntu, CRI-O from its rpm repository; records that undo with dnf; a second run changes nothing; packages of another release refused with dnf's command; an upgrade moves the repository file to the new minor and takes kubeadm, then kubelet and kubectl, to the release.
- **`internal/prepare` (SELinux):** kubeadm sets SELinux to permissive now (`setenforce 0`) and at boot (`/etc/selinux/config`), only what enforced, and the undo enforces again as it did; k3s leaves it alone.
- **`internal/k3s` (SELinux):** a machine whose SELinux enforces gets `selinux: true` in its config.yaml (once, even when the file sets it), the others don't; a custom data-dir is refused there before anything changes.
- **`internal/config` (systems):** `spec.infrastructure.os` defaults to ubuntu-24.04 on aws, with the image's login user (ubuntu, ec2-user) unless the file names one; an unknown system, and the field on proxmox, byoh or local, are refused; `proxmox.cpuType` defaults to x86-64-v2-AES, and a value that isn't a CPU model's name is refused.
- **`internal/providers/tofu` (systems):** the AWS module gets the file's system (Ubuntu by default) and picks the newest image of it for each CPU architecture; Proxmox VMs get the file's CPU model, or the module's default.
- **`internal/prepare`:** what each distribution and component needs (from their docs), compared with what a machine has: packages, kernel modules and settings, swap, NetworkManager for Calico, ufw and firewalld rules; refusals (other systems, old kernels for Calico and Cilium, no memory cgroup with the Raspberry Pi fix, cloned MAC or product_uuid); the question before changing your machines; a failed change stops with the tool's output; the record on each machine. The generated scripts are checked with `sh -n`, and the read-only check runs in a local shell. ufw rules are compared as ufw reads them (any word order, a single address with or without /32, the short form), and the cluster's rules nothing needs any more are closed: never another cluster's, the user's own, or a rule in a form tow doesn't add, and nothing when this machine's address isn't known.
- **`internal/runtimes`:** containerd's support table (which releases go with which Kubernetes minor, 2.0 or newer only), CRI-O's minor matching Kubernetes', the pinned containerd and runc sums, and the embedded containerd unit being containerd's own, byte for byte.
- **`internal/kubeadm`:** against fake machines that answer its scripts: kubelet, kubeadm and kubectl from pkgs.k8s.io at the release and held; containerd from its release with the pinned sum (or the published one for another release), systemd cgroups and kubeadm's pause image; CRI-O from its repository for the minor, its drop-in written first; a second run changes nothing; a runtime already there is used when it goes with the release; refusals before any change on any machine (Docker's containerd.io, a release containerd doesn't recommend, a release other than the file's, a stopped runtime, another cluster's, CRI-O of another minor, k3s, a machine already in a cluster, packages of another release, an unsupported architecture); a failure names the machine with apt's output. The cluster itself: the init and join configs, compared in full (CRI socket, node IP, labels, taints and the control plane's own, kubelet flags, `extraArgs` with secrets filled in, certSANs, kube-proxy skipped when replaced); every config validated before anything starts; the token in kubeadm's form, made per run and deleted after the joins (also after a failed one); the CA hash workers check; the kubeconfig pointed at the API address; role labels; a second run changes nothing; a failed init or join (even one that left kubeadm's files and a running kubelet) is reset and retried on the next run, with a new token, leaving finished machines alone; a failure shows kubeadm's output, the kubelet's log and the containers; refusals (another cluster's node, an unknown pool). Several control planes: init uploads the certificates with a key tow makes, the others join one at a time with it (their config compared in full), then the workers; the certificates and the token are deleted after the joins, also after a failed one; a control plane that failed is reset and joins on the next run with the certificates uploaded again; the API's certificate names every control plane; your own load balancer is tested from the first control plane before init, and a timeout stops everything. kube-vip: its static pod goes on the first control plane after any reset and before `kubeadm init`, reading `super-admin.conf`, then `admin.conf` once init is done, and on the others once they joined, never on workers; the virtual IP is the `controlPlaneEndpoint`, in the certificate and the kubeconfig; one off the control planes' network is refused before anything changes. Every script is checked with `sh -n`.
- **`internal/kubevip`:** the static pod and the DaemonSet decode as Kubernetes objects and carry what `kube-vip manifest pod|daemonset` prints at the pinned release (environment, capabilities, host alias, kubeconfig mount, control planes only); the image is pinned by digest; the interface is found from `ip -o -4 addr show` (VLANs too, the virtual IP already held too) and a virtual IP off its network is refused; the write script runs in a local shell, writes root-only, and leaves an unchanged manifest alone.
- **`internal/core`:** also:
  - machines are prepared after they're created or checked, with a question only for your own; a failed preparation keeps the machines and the next apply resumes without recreating them
  - k3s goes on prepared machines, and the kubeconfig is saved (owner-only) and merged
  - a failed install resumes on the same release, even if "stable" moved
  - an existing kubeconfig context is refused before anything starts
  - secret variables are read first and never saved
  - kubeadm machines get their runtime and packages after preparation (whose own check now includes the runtime), then the cluster, the kubeconfig and the CNI; a failed setup or install resumes; "stable" is checked against the runtime once resolved, before anything is created; several kubeadm control planes are refused before anything happens; `StopAfter` (tests only) leaves machines prepared with nothing on them
  - delete removes the context, and on your own machines removes Kubernetes (see "delete on your machines" above)
  - Calico and Cilium go on right after Kubernetes, and flannel too on kubeadm (kubectl for Calico and flannel, matched to the cluster's release), then every node must be Ready; a failed CNI keeps the machines and resumes; the address the CNI reaches the API on (the private one with one control plane)
  - Helm charts go on every cluster, after helm is found; no helm means nothing is created; a failed chart keeps the cluster and the next apply installs the charts again (a k3d cluster the file changed is built again); chart secrets are read first and never saved
- **`internal/remote`:** SSH, with host keys pinned per cluster.
- **`internal/ui`:** secrets are redacted from every output, including streamed OpenTofu logs.

## 2. Local k3d cluster

```bash
make test-integration
```

Three tests; every cluster they create is deleted at the end, whatever happens. `KUBECONFIG` points to a temporary file, so your own kubeconfig is never touched.

- `TestK3dLifecycle` checks that Docker and k3d are installed, creates a bare k3d cluster named `test-<number>`, and waits until it reports healthy.
- `TestK3dFromFiles` runs `apply` on four files under throwaway names, then runs the [cluster checks](#cluster-checks) through the API's service proxy (k3d's nodes live in Docker). When this machine has no helm, the test downloads the pinned release into a throwaway folder first, never into `~/.local/bin`.

  | File | Must run | Must not exist |
  |---|---|---|
  | `local-helm.yaml` (flavor `local` + the podinfo chart) | the Helm release deployed; podinfo with 2 ready replicas, answering through its Service | |
  | `custom-bare.yaml` (no flavor) | CoreDNS; nodes Ready on flannel | traefik, servicelb, local-path, metrics-server, Gateway API CRDs |
  | `local.yaml` (flavor `local`) | CoreDNS, traefik, servicelb, local-path, metrics-server, Gateway API CRDs | |
  | a server and a worker with a role label and a taint | CoreDNS; the label (set through the API) and the taint on the worker | traefik, metrics-server |

  Each cluster's context must be in the kubeconfig after `apply`, and `delete` must leave no cluster, context or state.

- `TestK3dAddonChanges` changes a running cluster's own add-ons through `apply`, one file after another, checked through the API and in state: the podinfo chart (1 replica) and a manifest of two ConfigMaps added; the same file again changes nothing; 2 replicas and one ConfigMap dropped from the manifest (it's deleted, the other stays); the manifest's namespace deleted by hand (the same file puts it back, the ConfigMap with it); both removed from the file (the release uninstalled, the ConfigMap deleted, both namespaces kept). It also downloads the kubectl of the cluster's minor release into the throwaway folder.

### Cluster checks

Every test that builds a cluster ends with the same checks (`checkCluster` in `integration/`). They run through the kubeconfig tow handed out, from this machine, so they prove it works:

- **What the file asks for, and nothing else:**
  - every node registered under its name, with its role and the release asked for
  - each pool's labels (role labels included) and taints
  - the control planes tainted or not, as `schedulable` says
  - k3s and k3d: each bundled component asked for is running, and the others don't exist
  - kubeadm: etcd, the API server, the controller manager, the scheduler and CoreDNS run; kube-proxy runs unless the CNI replaces it; none of k3s's extras exist
  - each Helm chart the file asks for is deployed
  - Calico's, Cilium's or (on kubeadm) flannel's DaemonSet Ready on every node. flannel's config has the cluster's pod range and the file's backend. When asked for: kube-proxy's job taken over (Cilium's `kube-proxy-replacement`, Calico's BPF dataplane) and WireGuard keys published by every node.
- **Storage** (`checkStorage`): each driver's StorageClass exists and the file's default is the only default. In a throwaway namespace, a pod writes into a new 1 GiB volume of each class and another pod reads it back, on another node for networked storage (Longhorn, NFS, SMB). An NFS or SMB server the file names as an in-cluster Service (`<name>.<namespace>.svc.cluster.local`) is started first from the drivers' own example servers, so the lab needs no NAS.
- **Ingress** (`checkIngress`): every IngressClass in the cluster (built in, or from a chart the file installs) serves an Ingress from this machine, at the address its controller publishes in the Ingress's status; built-in Traefik without a load balancer on a node's port 80.
- **Gateway** (`checkGateway`): every GatewayClass serves a Gateway and an HTTPRoute from this machine, at the Gateway's address.
- **Firewall ports** (`checkAccessPorts`): a pod listening on each of the file's `access.ports` (a node's port) answers at that node's address.
- **Cloud volumes and delete** (`keepCloudVolume`, with EBS or Proxmox disks): a volume left in use makes a delete without `-v` refuse, naming it; the test's own delete then uses `-v`, and on AWS the leftover check proves the volume is gone.
- **Service load balancer** (`checkServiceLB`), when tow installs one: a LoadBalancer Service gets an address from the file's pool, and answers HTTP there from this machine.
- **metrics-server** (`checkMetrics`): the metrics API reports every node. On kubeadm this also proves the kubelets' serving certificates were signed and approved.
- **Manifests** (`checkManifests`): each manifest's namespace exists, marked as tow's, with the ConfigMaps a local manifest names in it.
- **A hello world**, in a throwaway namespace that's always deleted. It uses the Kubernetes project's test image, `agnhost`.
  - Two replicas run, on different nodes when two can take them.
  - The test calls one replica from this machine: through a NodePort on a machine's address, which proves the firewall rules, or through the API's service proxy on k3d.
  - That replica calls the ClusterIP Service by its DNS name until both replicas have answered. This proves pod networking across nodes, cluster DNS and Service routing.
  - It then calls each replica directly by its pod IP: pod-to-pod traffic across nodes through the CNI alone.
- **Status** (`checkStatus`): `tow status` says healthy once the checks' own pods are gone.
- **Failover**, for clusters of several control planes (`checkFailover`): a control plane is powered off through the cloud's API: on AWS the first one, on Proxmox the one holding kube-vip's virtual IP (the holder of its Lease, `kube-system/plndr-cp-lock`). The API must still answer through the address in front of the control planes, the rest of the control plane must see the missing one NotReady, and a new hello world must run and answer (scheduling, DNS, Services, a NodePort on a machine that stays up). While it's off, `tow status` must say not healthy, with that control plane NotReady. Then it's powered on and must be Ready again. With kube-vip, the kubeconfig must point at the virtual IP, and another control plane must hold it afterwards. First, CoreDNS's replicas must run on different nodes.
- **CoreDNS on k3s with several control planes** (`checkK3sCoreDNS`, Proxmox): two replicas Ready on different machines; then k3s is restarted on the last control plane, which makes it apply its own CoreDNS manifest again (seen by the Addon's `AppliedManifest` event), and CoreDNS must still have two replicas.

## 3. AWS

Use credentials from any standard source: `aws sso login`, `aws configure` (pick one with `AWS_PROFILE=<name>`), or `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`. Check them first:

```bash
aws sts get-caller-identity
```

Optional settings for both tests:
- `TOW_TEST_REGION`, default `eu-west-3`.
- `TOW_TEST_AWS_CONFIG=../examples/ebs-aws.yaml`: EBS volumes, Traefik on the nodes' ports 80/443 and `access.ports` through the security group, then delete with `-v` (~$0.03).
- `TOW_TEST_AWS_CONFIG`, `make test-aws` only: a cluster file to build instead of the small dev-test one, under a throwaway name, in its own region. `../examples/storage-aws.yaml` installs every storage driver, metrics-server and a manifest on three t3.small (~25 minutes, about $0.04), with the NFS and SMB test servers the checks start. `../examples/cnpg-aws.yaml` runs Postgres with CloudNativePG on three workers in three zones (~12 minutes, about $0.05): the test writes a row, powers off the primary's machine, waits for a replica in another zone to take over, reads the row back through the Service `db-rw`, then powers the machine on again.
- `TOW_TEST_INSTANCE_TYPE`, default `t3.small`, the smallest size kubeadm accepts.
- `TOW_TEST_KUBERNETES`, `make test-aws` (and `make test-proxmox`): YAML merged into the cluster's `spec.kubernetes`, to try other choices. Its keys win; the rest of the file stays. For example:

  ```bash
  TOW_TEST_KUBERNETES='{containerRuntime: cri-o, networking: {cni: flannel}}' make test-aws
  TOW_TEST_KUBERNETES='{networking: {cni: cilium, cilium: {kubeProxyReplacement: true, encryption: wireguard}}}' make test-aws
  TOW_TEST_KUBERNETES='{networking: {cni: calico, calico: {dataplane: ebpf}}}' make test-aws
  ```

Cleanup has a third layer on AWS: the test machines shut down and terminate themselves after one hour (`TOW_RESOURCE_TTL=1h`), even if the test process is killed.

### `make test-aws`

`TestAWSInfra` runs `tow apply` on a dev-test cluster: kubeadm with Calico, one control plane and one worker, in a new VPC. It then logs in to each server and checks:

| Check | What it proves |
|---|---|
| OS is Ubuntu, cloud-init finished, `sudo -n true` works | the server is ready for Phase 2 |
| at least 2 CPUs and ~1.7 GB of memory | kubeadm will accept it |
| DNS, HTTPS out, `apt-get update` | it can download what Phase 2 installs |
| the metadata service refuses requests without a token (401) | IMDSv2 only, which blocks credential theft |
| a shutdown is scheduled | the one-hour self-destruct is armed |
| curl and ca-certificates installed, swap off, `net.ipv4.ip_forward = 1` now and in `/etc/sysctl.d/k8s.conf`, `/etc/tow/prepared.json` lists each change with its undo | the machine was prepared as the kubeadm docs ask, and it's recorded |
| kubelet, kubeadm and kubectl at the cluster's release and held, the kubelet enabled; containerd 2.3.7 running, its config (as `containerd config dump` reads it) with `SystemdCgroup = true` and kubeadm's pause image; `crictl info` answers on containerd's socket; both recorded with their undo | the machine has what kubeadm needs, as its docs ask |
| `/etc/tow/kubeadm-done` names the cluster, the kubelet runs, the kubeadm run is recorded with its undo; on the control plane, tow's join token is gone | kubeadm init or join finished here, for this cluster |
| ping and TCP to the other node on its private IP | nodes can reach each other |

Then the [cluster checks](#cluster-checks) run, with the API and a NodePort reached on the control plane's public address. When this machine has no helm or kubectl, the test downloads the pinned releases into a throwaway folder.

Last, it runs `tow delete`, which fails if AWS still lists anything tagged `tow-cluster=<name>`, and waits for AWS to report both machines terminated (see the exit codes above).

### `make test-aws-ha` (opt-in)

`TestAWSHA` does what `make test-aws` does with three control planes and a worker, behind the two NLBs tow creates (a public one for you, an internal one for the nodes): kubeadm by default, k3s with `TOW_TEST_KUBERNETES='{distribution: k3s}'`. On top:
- kubeadm's control planes join one at a time with the certificates init uploaded, behind the internal NLB; the cluster checks reach the API through the public NLB, and a NodePort on the worker.
- The [failover check](#cluster-checks) stops the first control plane through AWS, then starts it again (its self-destruct, armed at first boot only, is armed again).

The load balancers don't destroy themselves after the hour like the machines do: if the test process dies, run the `tow delete` its last lines name. About $0.07 a run (two NLBs: a public one the test reaches the API through, an internal one the nodes use).

### `make test-byoh-aws`

`TestBYOHOnAWS` creates two throwaway servers with the AWS module (whose apply stops once they exist: nothing prepared, no Kubernetes), then hands them to byoh as if they were your own machines, with `--yes` standing in for your answer. It runs three cases, in this order, on the same two servers, and ends like `make test-aws`: the servers deleted, then confirmed terminated. About 30 minutes, ~$0.05.

| Case | What it proves |
|---|---|
| **blocked node-to-node port is caught** | after the worker's firewall drops the control plane's traffic to port 10250, byoh fails with "node-to-node: can't reach … port 10250", and only for that port (6443 still works). The rule is removed afterwards. Then ufw is turned on on both servers (SSH allowed), as on many servers. |
| **k3s, then delete --reset** | with the `homelab` flavor, byoh finds each machine's private IP and a node name by itself, prepares the machines (its ufw rules among the changes) and installs k3s. The [cluster checks](#cluster-checks) pass, with the API and a NodePort reached on the control plane's public address. Each machine's `/etc/tow/prepared.json` lists Kubernetes and the firewall with revert scripts. `delete --reset -v` then leaves nothing tow added: no k3s, no tow ufw rule (ufw still on), none of the packages it installed, no `k8s.conf` files, no `/etc/tow`; the kubeconfig context is gone. |
| **kubeadm, then delete --reset** | on the machines the k3s case handed back, with swap turned on first: the `dev-test` flavor (kubeadm, Calico), whose preparation turns swap off. Same checks, and after `delete --reset -v` no kubelet, kubeadm, kubectl, containerd or runc, no `/etc/cni/net.d` or `/etc/containerd`, swap on again and its `/etc/fstab` line back. |

`TOW_TEST_OS=almalinux-9 make test-byoh-aws` runs the same cases on AlmaLinux 9 servers, with RHEL's tools: firewalld instead of ufw (its zone `tow-nodes` and policy `tow-admin` gone after `--reset`, firewalld still on), packages removed with dnf, `/etc/yum.repos.d/kubernetes.repo` gone, and SELinux, which kubeadm's preparation set to permissive, enforcing again now and at boot.

## 4. Proxmox VE

You need a Proxmox server with an Ubuntu cloud-init template and an API token. [proxmox.md](proxmox.md) covers the setup, including a Hyper-V lab on a Windows PC.

```bash
export PROXMOX_VE_API_TOKEN='tow@pve!tow=<secret>'   # type or paste it in your own terminal only
export PROXMOX_VE_INSECURE=true                                 # self-signed lab certificate only
make test-proxmox
```

`TestProxmoxInfra` builds the cluster in `TOW_TEST_PROXMOX_CONFIG` (default `examples/dev-test-proxmox.yaml`: two VMs of 2 GB) under a throwaway name.

1. Before creating anything, tow checks the token, the node, the template, and that no VM address is already in use.
2. On each VM, the test checks:
   - Ubuntu, cloud-init finished, passwordless sudo
   - the hostname is the VM name
   - at least 2 CPUs and ~1.7 GB of memory
   - DNS, HTTPS and the package mirrors are reachable
   - ping and TCP to the other node
   - prepared as the distribution's docs ask (packages; for kubeadm swap off and IPv4 forwarding), with each change in `/etc/tow/prepared.json`
   - with kubeadm: kubelet, kubeadm and kubectl at the cluster's release and held, and the file's container runtime (containerd or CRI-O) at its release, set up for Kubernetes, answering `crictl info`, and recorded; kubeadm finished there for this cluster, as on AWS
3. The [cluster checks](#cluster-checks) run, with a NodePort reached on the first VM's address. Files for the lab:
   - kubeadm: `dev-test-proxmox.yaml` (Calico), the default; other choices through `TOW_TEST_KUBERNETES` ([AWS](#3-aws) has examples), like `'{networking: {cni: flannel}}'` or `'{containerRuntime: cri-o}'`
   - k3s: `homelab-proxmox.yaml` (flannel), `calico-proxmox.yaml`, `calico-ebpf-proxmox.yaml`, `cilium-proxmox.yaml` (kube-proxy replacement + WireGuard)
   - several control planes behind kube-vip: `homelab-proxmox-ha.yaml` (three VMs of 2 GB, kubeadm + flannel; k3s with `TOW_TEST_KUBERNETES='{distribution: k3s}'`), then the [failover check](#cluster-checks)
   - storage, metrics-server and manifests: `storage-proxmox.yaml` (three VMs of 2 GB, kubeadm + flannel; k3s the same way), with the NFS and SMB test servers the checks start
   - ingress: `ingress-proxmox.yaml` (kubeadm + MetalLB: Traefik built in, F5 NGINX, HAProxy and Envoy Gateway from their charts, Contour from its manifest), `cilium-ingress-proxmox.yaml` (k3s + Cilium's Ingress and Gateway), and `homelab-proxmox.yaml` (k3s's own Traefik through servicelb)
   - Proxmox disks as volumes: `proxmox-csi.yaml`, with `PROXMOX_CSI_TOKEN` set and Proxmox in a cluster ([docs/storage.md](../storage.md))
   - service load balancers: `metallb-proxmox.yaml` (kubeadm), `kube-vip-lb-proxmox.yaml` (k3s, kube-vip's own DaemonSet), `cilium-lb-proxmox.yaml` (k3s, Cilium L2); and kube-vip in front of the API announcing Services too: `homelab-proxmox-ha.yaml` with `TOW_TEST_KUBERNETES='{serviceLoadBalancer: {provider: kube-vip, kube-vip: {addresses: [192.168.100.200-192.168.100.210]}}}'`. BGP needs a router the lab doesn't have: unit-tested only.

   When this machine has no helm or kubectl, the test downloads the pinned releases into a throwaway folder.
4. `delete` must leave no VM tagged `tow-cluster.<name>`, and no kubeconfig context.

Expected in a lab: a warning that the certificate check is off, and another if the Datacenter firewall is off (turn it on: the tests then prove tow's firewall rules too). cloud-init also reports "recoverable errors": the configuration Proxmox generates uses a deprecated `user:` key. The test logs this instead of failing.

`production-proxmox.yaml` isn't run: its six VMs (3 × 4 GB and 3 × 8 GB, about 36 GB) need more than the lab described in [proxmox.md](proxmox.md). Run it on a real server.

### `make test-lifecycle`: a cluster's whole life

`TestProxmoxLifecycle` drives one cluster through every change a running cluster takes, the cluster checked after each stage, stopping at the first that fails (each builds on the one before). `DIST=k3s`: three control planes behind kube-vip (schedulable) and a worker; `DIST=kubeadm`: one control plane behind kube-vip and a worker, Cilium replacing kube-proxy. Both start at the previous minor release, with CloudNativePG's operator from the file.

| Stage | What it proves |
|---|---|
| create | the cluster checks, as for `make test-proxmox` |
| re-apply unchanged | "nothing to do" |
| a worker added, then removed | the dry run shows Infrastructure, then Cluster, and replaces nothing; the worker joins, then is drained and removed |
| the control planes change | k3s: one removed and added back; kubeadm: a second added (the lab holds one more VM), then removed; an even number is warned about |
| add-ons on and off | a Helm chart (podinfo) installed, then uninstalled |
| the admin CIDRs | an extra CIDR added, then removed: the firewalls follow, this machine keeps its access |
| new settings | an API server flag and the worker's max-pods (150): no drain |
| a marker, then a backup | `tow backup` |
| the upgrade | a pod pinned to the worker under a budget allowing no disruption: the drain times out (2 min), the error names the budget and the nodes already upgraded; the budget removed, the next apply skips them and finishes; every node on the new release; Postgres (CloudNativePG, two instances, asynchronous replication, the readiness probe `streaming` with a maximum lag, `enablePDB: false` as its docs advise for drains) written to every few seconds throughout: every committed row is there after, and the failed writes are counted; the snapshot taken first is on this machine, whole (0600, the size its .json records) |
| the pre-upgrade snapshot restored | the marker from before the upgrade is back, the one after it gone (kubeadm: skipped when the upgrade moved etcd's minor release, which `restore` refuses) |
| certificates renewed | `--renew-certificates`: the kubeconfig is fetched again |
| delete | nothing left |

On kubeadm, the stages k3s's run already covers (a worker, add-ons, the admin CIDRs, settings) are skipped, so it takes about 35 minutes.

A failed run keeps its cluster and says how to carry on: `TOW_TEST_LIFECYCLE_NAME=<its name> TOW_TEST_LIFECYCLE_FROM="<a stage's title>" make test-lifecycle DIST=…` reuses it and starts at that stage (the file edits of the stages before are still made, so the file matches the cluster; a stage this run never runs leaves none). Fix the test that way, on the running cluster; the full run is the last proof.

`TOW_TEST_LIFECYCLE_WORKERS=2` starts the cluster with two workers: Postgres's instances then sit on two workers drained one after the other, the case tow's wait before each drain is for (kubeadm on the lab: three VMs, so the control-plane stage is left out).

**`make test-lifecycle-conformance`** runs the kubeadm lifecycle with two workers (the conformance tests want them) and without the control-plane stage (the lab holds no fourth VM), then, after the certificates are renewed, Kubernetes' conformance tests as `make test-conformance` does: on a cluster that was upgraded and restored. The results stay in `integration/conformance/`.

**`make test-aws-lifecycle`** (`TestAWSLifecycle`) runs the stages only AWS can show, on a kubeadm cluster of two t3.small (Cilium replacing kube-proxy): create, re-apply unchanged, a worker added and removed, new settings, the upgrade, certificates renewed, delete. Adding the worker also proves OpenTofu's part: each machine keeps its server and zone (read from its own metadata), and the new one goes to a zone none of them uses. The cluster is deleted even after a failure, unlike on the lab (`TOW_TEST_LIFECYCLE_KEEP=1` keeps it; its machines shut themselves down after 2 hours anyway). `TOW_TEST_REGION` and `TOW_TEST_INSTANCE_TYPE` work as for `make test-aws`.

`proxmox-matrix.txt` has a `lifecycle:k3s` and a `lifecycle:kubeadm` line: `make test-proxmox-all` runs them too.

### `make test-proxmox-all`: every case, one at a time

[`integration/proxmox-matrix.txt`](../../integration/proxmox-matrix.txt) lists every case, one per line: an example file, and optionally YAML merged into its `spec.kubernetes`. It covers the distributions and CRI-O, several control planes (kubeadm, and k3s with kube-vip announcing Services), every network and its options (Calico's encapsulations, Cilium as it ships, flannel's backends, `cni: none`), storage with a manifest folder, Proxmox disks, the service load balancers and ingress.

```bash
export PROXMOX_CSI_TOKEN='kubernetes-csi@pve!csi=<secret>'   # for proxmox-csi.yaml
make test-proxmox-all                 # 3 to 4 hours; nothing asks anything, so it can run overnight
make test-proxmox-all ONLY=calico     # only the lines that contain "calico": to run again what failed
```

Each line runs `TestProxmoxInfra` from one test binary, built once, and a failed line doesn't stop the others. Each line's output goes to `integration/logs/<date>/`, with `summary.txt`: PASS or FAIL and the minutes, line by line. The exit code is 1 if any line failed. `make test` checks that every line is a valid cluster file, so a typo fails in seconds rather than hours later.

### `make test-conformance` (opt-in)

Runs Kubernetes' conformance tests with [Sonobuoy](https://sonobuoy.io) 0.57.5 (pinned, its archive checked against its sha256), in the `certified-conformance` mode CNCF's [Certified Kubernetes](https://www.cncf.io/training/certification/software-conformance/) program uses, on a cluster `TestProxmoxInfra` builds: `custom-proxmox.yaml` by default (kubeadm, two workers: some tests need two nodes that take workloads). About two hours. Any failed test fails it. The results stay in `integration/conformance/<cluster>-<release>/`: Sonobuoy's archive and `results.txt`, what a submission to CNCF is made from.

## The cluster file, for writing your own test files

Every field, in one place. Only `apiVersion`, `kind`, `metadata.name` and `spec.infrastructure.provider` are always required; everything else has a default. `flavor` is optional and only fills in what the file leaves out.

This is a reference, not a file to apply: it shows the fields of every provider at once, so tow would refuse it as written (the table below says which fields go with which provider). For files that work, start from `examples/`.

```yaml
apiVersion: towproj.github.io/v1alpha1
kind: Cluster
metadata:
  name: lab                          # DNS label: names the machines, the state and the cloud tags

spec:
  flavor: homelab                    # optional preset: local | homelab | dev-test | production

  kubernetes:                        # what gets installed: exactly this, nothing else (every field: examples/custom-full.yaml)
    distribution: k3s                # k3s | kubeadm | k3d (local only); required when there's no flavor
    version: v1.37.2                 # left out: v1.37.2, the release tow tests; "stable": the newest; k3s: v1.37.2+k3s2 for a later k3s build
    # containerRuntime: containerd   # kubeadm only: containerd (default) | cri-o; k3s brings its own containerd
    # containerd: {version: 2.3.7}   # left out: 2.3.7; a release containerd recommends for the Kubernetes minor
    apiEndpoint: 192.168.1.100:6443  # required with >1 control plane on byoh and proxmox; refused on aws (it makes a load balancer)
    apiLoadBalancer: kube-vip        # what serves apiEndpoint: kube-vip (default) | external (byoh only)
    networking:                      # defaults: k3s/k3d 10.42.0.0/16 + 10.43.0.0/16, kubeadm 10.244.0.0/16 + 10.96.0.0/12
      podCIDR: 10.42.0.0/16
      serviceCIDR: 10.43.0.0/16
      clusterDomain: cluster.local
      cni: cilium                    # flannel | calico | cilium | none; default none (local: flannel); decides the ports between nodes
    storage: {csi: [local-path]}     # several drivers; defaultClass names the default
    serviceLoadBalancer: {provider: servicelb}   # exactly one; pools need addresses
    ingress: {controllers: [traefik]}
    metricsServer: {enabled: true}
    apiServer: {extraArgs: {audit-log-maxage: 30}}   # also controllerManager, scheduler, etcd, kubelet; k3s.server / k3s.agent on k3s
    addons: {helmCharts: [], manifests: []}
    kubeconfig: {context: lab, merge: true}          # defaults: metadata.name, merged into ~/.kube/config

  infrastructure:
    provider: proxmox                # aws | gcp | azure | proxmox | byoh | local
    os: ubuntu-24.04                 # clouds only: ubuntu-24.04 (default) | almalinux-9 | almalinux-10
    aws:                             # provider aws only
      region: eu-west-3
    gcp: {project: my-project-123456, region: europe-west1}   # provider gcp only; Application Default Credentials
    azure: {subscription: 00000000-0000-0000-0000-000000000000, location: swedencentral}   # provider azure only; az login or ARM_* variables
    proxmox:                         # provider proxmox only; the token comes from PROXMOX_VE_API_TOKEN
      url: https://192.168.100.10:8006
      node: pve
      template: 9000
      storage: local-lvm             # default
      bridge: vmbr0                  # default
      cpuType: x86-64-v2-AES         # default; x86-64-v3 or host for a RHEL 10-based template
    network:                         # proxmox: all four fields; clouds: subnet only (the VPC/VNet, default 10.250.0.0/16)
      subnet: 192.168.100.0/24
      addresses: 192.168.100.20-192.168.100.29   # VMs take them in order, control planes first
      gateway: 192.168.100.1
      dns: [1.1.1.1]
    access:
      ssh: {user: ubuntu, keyPath: ~/.ssh/id_ed25519, port: 22}   # port: byoh only
      adminCIDRs: [203.0.113.7/32]   # clouds/proxmox: who reaches SSH and 6443; default: this machine
      ports: [8080]                  # more TCP ports open to adminCIDRs, for what you install yourself

  controlPlane:
    count: 1                         # clouds, proxmox, local (k3d servers)
    instanceType: t3.small           # clouds: the cloud's own size names (e2-medium, Standard_B2s_v2)
    resources: {cpu: 2, memory: 2Gi} # proxmox
    disk: 20Gi                       # clouds, proxmox
    machines:                        # byoh only, instead of count
      - host: 192.168.1.10
        ssh: {user: admin}           # per-machine override of access.ssh
    schedulable: auto                # control plane only: auto = workloads here only without workers
    labels: {zone: rack-1}           # every pool: labels, taints, kubelet: {extraArgs}
    taints: [{key: dedicated, value: control, effect: NoSchedule}]

  workers:                           # pools; a single pool may omit its name (it becomes "workers")
    - name: general
      count: 2                       # local: one pool only, its count is k3d's agents
      instanceType: t3.large
      disk: 50Gi
```

What each provider accepts. A field outside its column is refused, with the reason:

| Field | `aws` | `proxmox` | `byoh` | `local` |
|---|---|---|---|---|
| `kubernetes.distribution` | k3s, kubeadm | k3s, kubeadm | k3s, kubeadm | k3d (implied) |
| `networking.cni` | ✓ | ✓ | ✓ | flannel or none |
| `networking` ranges, `clusterDomain` | ✓ | ✓ | ✓ | refused (k3d brings its own) |
| storage, ingress, service load balancer | all but `proxmox` storage | all but `aws-*` | all but `aws-*`, `proxmox` storage | k3s's own: local-path, traefik, servicelb |
| `kubernetes.apiEndpoint` | refused (load balancer) | ✓, required with >1 control plane | ✓, required with >1 control plane | refused |
| `kubernetes.apiLoadBalancer` | refused | kube-vip | kube-vip, external | refused |
| `apiServer.certSANs` | ✓ | ✓ | ✓ | refused (k3d sets them) |
| `infrastructure.network` | `subnet` only | ✓ required | refused | refused |
| `access.ssh` | user, keyPath | user, keyPath | user, keyPath, port | refused |
| `access.adminCIDRs` | ✓ | ✓ | refused (not yet) | refused |
| `count` | ✓ | ✓ | refused | ✓ |
| `instanceType` | ✓ | refused | refused | refused |
| `resources` | refused (not yet) | ✓ | refused | refused |
| `disk` | ✓ | ✓ | refused | refused |
| `machines` | refused | refused | ✓ required | refused |

Limits that apply everywhere:
- **Sizes:**
  - At least 1 control plane.
  - A listed worker pool needs at least 1 machine.
  - Sizes are written as quantities (`512Mi`, `4Gi`, `1Ti`); a bare number like `4` is refused.
  - Provisioned machines can't be smaller than the distribution's minimum for their role. A kubeadm control plane needs 2 CPUs, 2 GB of memory and a 14 GB disk. On AWS, instance types are checked through the API before anything is created.
- **Address ranges:** the pod and service ranges must not overlap each other or the machines' addresses. Load balancer pools must not overlap them either, nor the API address or each other; on Proxmox a layer-2 pool must be inside the subnet and outside the VM range. Layer-2 pools are refused on AWS.
- **Components:** each one only where it works (servicelb on k3s and k3d, Cilium's load balancer and ingress with `cni: cilium`, `aws-*` on AWS, `proxmox` storage on Proxmox). Options for a component that isn't selected are refused. Components k3s bundles take no `version` on k3s and k3d.
- **Pass-through:** `extraArgs` flags are checked for shape only, plus conflicts with fields of the file or flags tow sets. Helm charts need `repo`, `chart` and an exact `version`; local manifests must exist.
- **Warnings, not refusals:**
  - an even number of control planes
  - no workers
  - no CNI (nodes stay NotReady)
  - several storage drivers or ingress controllers and no default class
  - a secret flag whose value can still be seen somewhere (kubeadm's process list, k3s's log, `docker inspect` on local clusters)
  - what this version doesn't install yet

## 5. By hand, with the CLI

Build first with `go build -o tow`. Every cluster follows the same four steps:

```bash
./tow apply -f <file> --dry-run   # shows what would happen; creates nothing, writes no state
./tow apply -f <file>             # does it; state goes to ~/.tow/clusters/<name>/
./tow status --name <name>
./tow delete --name <name>        # asks for confirmation
```

| File | What you need | What to expect |
|---|---|---|
| `examples/local.yaml` | Docker, k3d | a k3d cluster `dev` with traefik, servicelb, local-path and metrics-server; `kubectl cluster-info --context dev` works |
| `examples/custom-bare.yaml` | Docker, k3d | a k3d cluster `bare` with flannel and nothing else; add `kubernetes: {networking: {cni: none}}` and the nodes stay NotReady, with a warning |
| `examples/custom-full.yaml` | nothing (dry run) | every field of the Kubernetes part, accepted; on placeholder hosts, byoh then reports that it can't reach them |
| `examples/homelab.yaml`, `dev-test.yaml`, `production.yaml` | your own machines: replace the placeholder hosts | byoh checks every machine and reports each problem with a hint; delete only forgets the cluster |
| `examples/dev-test-aws.yaml` | AWS credentials | two t3.small servers, SSH open to your public IP only; delete removes everything (~$0.06/hour while it runs) |
| `examples/dev-test-proxmox.yaml` | Proxmox token, lab addresses | two VMs; the dry run shows your address on the VM network (e.g. `192.168.100.1/32`) |
| `examples/custom-proxmox.yaml` | Proxmox token, lab addresses | no flavor: kubeadm + flannel, one control plane and two worker pools of different sizes (3 VMs) |
| `examples/custom-homelab-ha.yaml` | your own machines | no flavor: k3s + Cilium with 2 control planes; apply warns that 2 tolerate no failure |

Also worth trying by hand, because each one should be refused with a clear message:
- **A file that changed after apply:** change a worker pool's `count` and apply again. The answer is a drift diff, refused.
- **A field the provider doesn't use:** for example `instanceType` on Proxmox, or `network` on a local cluster. The answer names the field and why.
- **The same name in another mode:** for example, provision a cluster and then apply a byoh file with the same name. The answer is "can't change mode in place".
- **A machine another cluster already uses:** list it in a second byoh file. The answer is "already belongs to cluster …".
- **A key named like `apiToken:` or `password:` in the file:** the file is refused before anything runs.
- **A secret flag written in the file** (`etcd-s3-secret-key: abc` under `k3s.server.extraArgs`): refused, pointing to `{env: VARIABLE}`. With `{env: VARIABLE}` and the variable unset, apply stops before creating anything and names it.
- **A flag the file already sets elsewhere** (`service-cluster-ip-range` under `apiServer.extraArgs`): "conflicts with spec.kubernetes.networking.serviceCIDR".
- **Options for a component you didn't pick** (`networking.cilium` with `cni: flannel`): "is set but cni is flannel".
- **A kubeconfig context that already exists** (`kubeconfig: {context: <one of yours>}`): refused before anything is created.
- **A Proxmox range that overlaps a running machine:** the answer is "already in use on the network".

To try byoh on Proxmox, make VMs outside tow. Clone template 9000 in the web UI (**right-click → Clone**), set its cloud-init IP and SSH key, then list those addresses in a byoh file. VMs that tow created belong to their own cluster, and byoh refuses them.

## If a test dies midway

Normally there's nothing to do. If a process was killed hard (`kill -9`, a closed laptop), look for leftovers:

```bash
ls ~/.tow/clusters/                     # a leftover test-* cluster: tow delete --name <it>
k3d cluster list                            # k3d: k3d cluster delete <test-...>
aws ec2 describe-instances --filters Name=tag-key,Values=tow-cluster \
  --query 'Reservations[].Instances[].[InstanceId,State.Name,Tags[?Key==`tow-cluster`]|[0].Value]' --output table
```

- **AWS:** `tow delete --name <cluster>` removes everything tagged for it, as long as its state folder still exists. Otherwise, delete the resources tagged `tow-cluster=<cluster>` in the console. Test machines terminate themselves after an hour anyway.
- **Proxmox:** search the web UI for VMs tagged `tow-cluster.<name>`, then use **More → Remove**. Nothing on Proxmox deletes them by itself.

## Secrets

```bash
make secrets    # scans the whole git history, like CI does
```

The pre-commit hook (`make hooks`) scans each commit before it's made. Test fakes that look like secrets carry a `// gitleaks:allow` comment. Never commit a real token, even in a test.
