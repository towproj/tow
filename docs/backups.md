---
title: "Backups: etcd snapshots"
linkTitle: Backups
weight: 70
---
# Backups: etcd snapshots

etcd holds everything the cluster's API knows: every object, every Secret, every setting. A snapshot of it brings that back. It doesn't hold your volumes' data. For that, see [App data](#app-data) below.

## Snapshots on a schedule (`spec.kubernetes.backups`)

```yaml
spec:
  kubernetes:
    backups:
      schedule: "0 */6 * * *"   # as cron: minute hour day-of-month month weekday
      keep: 10                  # the newest kept on each control plane; default 5
```

The schedule runs in the machines' time zone (UTC on cloud images). Without `backups`, tow adds no schedule of its own.

- **k3s** takes etcd snapshots itself: every 12 hours, keeping 5, in `/var/lib/rancher/k3s/server/db/snapshots` on each control plane. `backups` sets k3s's own `etcd-snapshot-schedule-cron` and `etcd-snapshot-retention`.
- **kubeadm** has no schedule of its own. `backups` puts a systemd timer (`tow-etcd-snapshot.timer`) on each control plane. It runs the `etcdctl` in the machine's own etcd container (through `crictl`), so nothing else is downloaded. The snapshots go to `/var/lib/tow/snapshots`, readable by root only. The timer is recorded in `/etc/tow/prepared.json` with its undo, and it goes when Kubernetes is removed from the machine. The snapshots stay.
  - systemd and cron read some schedules differently, so these are refused: a day of the month and a weekday both set (cron runs when either matches, systemd only when both do), and a step on a range (`0-12/2`). List the values instead.
  - `systemctl list-timers tow-etcd-snapshot.timer` shows the next run; `journalctl -u tow-etcd-snapshot` shows past ones.

### On a running cluster

- **kubeadm:** `apply` turns the timer on, changes it, or takes it off on every control plane. Taking it off leaves the snapshots already taken. A control plane added later gets the timer when it joins.
- **k3s:** a change is refused for now. k3s reads its schedule only when it starts, and restarting the control planes one at a time comes in a later version. `tow backup` takes a snapshot whenever you need one.

## A backup now: `tow backup`

```bash
tow backup --name prod          # a snapshot now, copied to this machine
tow backup --name prod --list   # the copies on this machine
```

`backup` takes a snapshot on the first control plane that answers (`k3s etcd-snapshot save` on k3s, the same `etcdctl` as the timer on kubeadm). The snapshot stays next to the scheduled ones and is pruned with them. A copy comes to this machine and is checked against the snapshot's sha256:

- **Where:** `~/.tow/backups/<cluster>/`, in a folder only you can read; each file is 0600.
- **What it is:** a `.json` beside each copy says the cluster, its Kubernetes release and distribution, the machine and time it was taken on, and its sha256.
- **It outlives the cluster:** `tow delete` leaves the copies and says where they are. On your own kubeadm machines, the timer's snapshots also stay in `/var/lib/tow/snapshots`, and delete names those machines.

A local (k3d) cluster has no snapshots.

## A snapshot holds your Secrets

Every Secret in the cluster is in its snapshots: in plain text, unless Secrets are encrypted at rest. Keep the snapshots as safe as the cluster's credentials. `backup` says so every time.

- **k3s:** `secrets-encryption: true` in `spec.kubernetes.k3s.server.extraArgs`, when the cluster is built.
- **kubeadm:** an `EncryptionConfiguration` for the API server. tow doesn't set one up yet.

## Copies somewhere else

The copy on this machine is your off-machine copy. tow uploads nothing anywhere itself.

- **k3s** can upload its own snapshots to S3 or any S3-compatible store. Its settings go through as written, with the keys read from environment variables when you run tow, never from the file:

  ```yaml
  spec:
    kubernetes:
      k3s:
        server:
          extraArgs:
            etcd-s3: true
            etcd-s3-endpoint: s3.eu-west-3.amazonaws.com
            etcd-s3-region: eu-west-3
            etcd-s3-bucket: my-cluster-snapshots
            etcd-s3-folder: prod
            etcd-s3-access-key: {env: S3_ACCESS_KEY}
            etcd-s3-secret-key: {env: S3_SECRET_KEY}
  ```

  They're k3s's own flags (https://docs.k3s.io/cli/etcd-snapshot), set when the cluster is built. `tow backup` uploads too, since `k3s etcd-snapshot save` reads the same settings.
- **kubeadm:** copy `~/.tow/backups/<cluster>/`, or the machines' `/var/lib/tow/snapshots`, with your own backup tool.

## Restoring

`tow restore --name <cluster>` puts the cluster's etcd back to its newest copy on this machine, or the one `--snapshot` names, on the same machines: [lifecycle](lifecycle.md#restoring-a-snapshot) says how, and what can be restored.

The machines' own scheduled snapshots have no `.json` beside them, so `restore` doesn't offer them: restore one with k3s's own `--cluster-reset --cluster-reset-restore-path` (https://docs.k3s.io/cli/etcd-snapshot), or etcd's `etcdutl snapshot restore` for kubeadm (https://kubernetes.io/docs/tasks/administer-cluster/configure-upgrade-etcd/#restoring-an-etcd-cluster).

## App data

etcd snapshots don't hold your volumes' data. Use your applications' own tools, installed through `addons.helmCharts`:

- **Velero** (https://velero.io/docs/) backs up objects and volumes to S3-compatible storage. A tested example comes with tow's lifecycle tests.
- **CloudNativePG** has its own backups to object storage and volume snapshots (https://cloudnative-pg.io/documentation/current/backup/). `examples/cnpg-aws.yaml` installs it.
