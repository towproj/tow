---
title: Testing on GCP
linkTitle: GCP
weight: 20
---
# Testing on GCP

The GCP tests create real machines in a project of yours, then delete them and check nothing is left. They cost a few cents a run; a free trial's credits cover them.

## Setup, once

1. **A project with billing** (a free trial works), and the two APIs tow uses:

   ```bash
   gcloud services enable compute.googleapis.com iam.googleapis.com --project <your-project-id>
   ```

   Enabling Compute Engine makes Google create a `default` VPC in the project. tow doesn't use it: each cluster gets its own VPC.
2. **Credentials:** Application Default Credentials, which tow and OpenTofu both read:

   ```bash
   gcloud auth application-default login
   ```

3. **The project to test in**, named in the environment (it never goes in the repository):

   ```bash
   export TOW_TEST_GCP_PROJECT=<your-project-id>
   export TOW_TEST_GCP_REGION=europe-west1   # the default
   ```

## The tests

| Test | Command | Machines | Time | Cost |
|---|---|---|---|---|
| A kubeadm cluster: every machine and cluster check, then delete | `make test-gcp` | 2 × e2-medium | ~15 min | ~$0.02 |
| The same on AlmaLinux | `TOW_TEST_OS=almalinux-10 make test-gcp` (or `almalinux-9`) | the same | ~15 min | ~$0.02 |
| Persistent disks (GCE PD CSI driver) and Traefik | `TOW_TEST_GCP_CONFIG=../examples/pd-gcp.yaml make test-gcp` (set the file's project first) | the same | ~15 min | ~$0.02 |
| Three control planes behind the load balancers; the first stopped, then started | `make test-gcp-ha` (k3s: `TOW_TEST_KUBERNETES='{distribution: k3s}'`) | 3 × e2-medium | ~30 min | ~$0.06 |
| A kubeadm cluster's life: a worker added in another zone and removed, new settings, the upgrade with Postgres written to, certificates renewed | `make test-gcp-lifecycle` | 2, then 3 × e2-medium | ~45 min | ~$0.05 |

Exit codes are the AWS tests' ([README.md](README.md)): 3 means machines may still be up, and the last lines say which and how to delete them.

## A free trial's limits

A free trial project allows **4 external IPv4 addresses** in use at once, and 8 E2 vCPUs per region; it can't ask for more. Each machine takes one address, and an HA cluster's public load balancer one more. So the HA test runs three control planes and no worker (the control planes run the workloads), and two tests with four machines between them can run at once, never more. Check what's in use:

```bash
gcloud compute project-info describe --project $TOW_TEST_GCP_PROJECT --format='table(quotas.metric,quotas.usage,quotas.limit)' | grep -E 'IN_USE_ADDRESSES|CPUS_ALL_REGIONS'
```

## If a test dies midway

The machines delete themselves an hour after they start (Compute Engine's maximum run duration, set when `TOW_RESOURCE_TTL` is). The rest (network, firewall rules, load balancers, the driver's service account) stays until deleted: `tow delete --name <cluster> --yes -v`, with the name the test printed. Everything tow makes in the project carries the label `tow-cluster=<cluster>` or the cluster's name at the start of its own.
