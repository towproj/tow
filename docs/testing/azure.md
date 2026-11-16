---
title: Testing on Azure
linkTitle: Azure
weight: 30
---
# Testing on Azure

The Azure tests create real VMs in a subscription of yours, then delete them and check nothing is left. They cost a few cents a run; a free account's credit covers them.

## Setup, once

1. **A subscription** (a free account works). Azure registers the resource providers tow uses (Microsoft.Compute, Microsoft.Network, Microsoft.ManagedIdentity, Microsoft.Authorization, Microsoft.DevTestLab for the test auto-shutdown) on first use; if one isn't, `az provider register --namespace <name>`.
2. **Credentials:** your Azure CLI login, which tow and OpenTofu both read:

   ```bash
   az login
   az account show --query id --output tsv      # the subscription's ID
   ```

   For a service principal (CI): `ARM_CLIENT_ID`, `ARM_TENANT_ID` and `ARM_CLIENT_SECRET`, which tow reads as OpenTofu's azurerm provider does.
3. **The subscription to test in**, named in the environment (it never goes in the repository):

   ```bash
   export TOW_TEST_AZURE_SUBSCRIPTION=<the subscription ID>
   export TOW_TEST_AZURE_LOCATION=swedencentral   # the default; westeurope takes no new customers
   ```

## The tests

| Test | Command | Machines | Time | Cost |
|---|---|---|---|---|
| A kubeadm cluster: every machine and cluster check, then delete | `make test-azure` | 2 × Standard_B2s_v2 | ~15 min | ~$0.03 |
| The same on AlmaLinux | `TOW_TEST_OS=almalinux-9 make test-azure` (or `almalinux-10`) | the same | ~15 min | ~$0.03 |
| Managed disks (Azure Disk CSI driver) | `TOW_TEST_AZURE_CONFIG=../examples/disk-azure.yaml make test-azure` (set the file's subscription first) | the same | ~15 min | ~$0.03 |
| Three control planes behind the load balancers; the first deallocated, then started | `make test-azure-ha` | 3 × Standard_B2s_v2 | ~30 min | ~$0.08 |
| A kubeadm cluster's life: a worker added in another zone and removed, new settings, the upgrade with Postgres written to, certificates renewed | `make test-azure-lifecycle` | 2, then 3 × Standard_B2s_v2 | ~45 min | ~$0.07 |

Exit codes are the AWS tests' ([README.md](README.md)): 3 means machines may still be up, and the last lines say which and how to delete them.

## A free account's limits

A free account allows few vCPUs per location (often 4, check yours): two Standard_B2s_v2 are 4. Check what's allowed and in use before the HA and lifecycle tests, which need 6:

```bash
az vm list-usage --location swedencentral --subscription $TOW_TEST_AZURE_SUBSCRIPTION --output table | grep -i -E 'Total Regional vCPUs|Standard BSv2 Family'
```

Raise it in the portal (Quotas → Compute), or pick smaller sizes for the workers with `TOW_TEST_INSTANCE_TYPE`.

## If a test dies midway

Everything of a cluster's is in its resource group, `tow-<cluster>`; the machines also deallocate themselves at the hour the test set (Azure's auto-shutdown). `tow delete --name <cluster> --yes -v` removes it all, or `az group delete --name tow-<cluster> --yes`.
