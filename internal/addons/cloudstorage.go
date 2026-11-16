package addons

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/providers/tofu/cloud"
	"github.com/towproj/tow/internal/ui"
)

// Sources (read 2026-11-11), followed here:
//   - AWS EBS CSI driver 1.68.0, its chart 2.68.0 (kubernetes-sigs.github.io/aws-ebs-csi-driver):
//     controller.extraVolumeTags, controller.nodeSelector and tolerations; the IAM permissions
//     come from AWS's managed AmazonEBSCSIDriverPolicy on the control planes (the AWS module),
//     and its pods read the instance metadata through a hop limit of 2
//     https://github.com/kubernetes-sigs/aws-ebs-csi-driver/blob/master/docs/install.md
//   - Proxmox CSI plugin 0.22.0, its OCI chart 0.5.15: config.clusters (url .../api2/json,
//     insecure, token_id, token_secret, region), storageClass; nodes labeled
//     topology.kubernetes.io/region (the cluster) and zone (the Proxmox node); a token of its
//     own with the CSI role; the csi-proxmox namespace allowed privileged pods
//     https://github.com/sergelogvinov/proxmox-csi-plugin/blob/main/docs/install.md

//   - GCE PD CSI driver 1.28.1: its "noauth" overlay (deploy/kubernetes/overlays/noauth at the
//     release tag, rendered with kubectl kustomize; gcp-pd.yaml.tmpl says what tow
//     changed): with no key, the controller uses Google's default credentials, the service
//     account of the control planes it runs on (pkg/gce-cloud-provider/compute/gce.go); disks
//     labeled with --extra-labels; StorageClass parameter type pd-balanced
//     https://github.com/kubernetes-sigs/gcp-compute-persistent-disk-csi-driver/tree/v1.28.1/deploy/kubernetes

//   - Azure Disk CSI driver, its chart 1.36.0 (raw.githubusercontent.com/kubernetes-sigs/
//     azuredisk-csi-driver/master/charts): the cloud config Secret azure-cloud-provider in
//     kube-system (useManagedIdentityExtension, userAssignedIdentityID, vmType standard),
//     controller.runOnControlPlane, controller.replicas, windows.enabled
//     https://github.com/kubernetes-sigs/azuredisk-csi-driver/blob/master/docs/install-csi-driver-master.md
//     https://github.com/kubernetes-sigs/cloud-provider-azure/blob/master/site/content/en/install/configs.md

const (
	EBSVersion        = "2.68.0" // the chart; the driver is 1.68.0
	AzureDiskVersion  = "1.36.0" // the chart; the driver is the same release
	ProxmoxCSIVersion = "0.5.15" // the chart; the plugin is 0.22.0
	GCPPDVersion      = "1.28.1" // the driver; no chart, its manifests are embedded

	ebsCharts       = "https://kubernetes-sigs.github.io/aws-ebs-csi-driver"
	proxmoxCSIChart = "oci://ghcr.io/sergelogvinov/charts/proxmox-csi-plugin"
	azureDiskCharts = "https://raw.githubusercontent.com/kubernetes-sigs/azuredisk-csi-driver/master/charts"
	controlPlane    = "node-role.kubernetes.io/control-plane"
)

// Proxmox is where the Proxmox CSI plugin's disks go, from the file and the environment.
type Proxmox struct {
	URL      string // the API address, https://host:8006
	Node     string // the Proxmox server the VMs run on: the nodes' zone
	Storage  string // where the disks go: local-lvm by default
	Insecure bool   // PROXMOX_VE_INSECURE: a lab's self-signed certificate
}

// ProxmoxCSIToken splits the plugin's token (id=secret, as PROXMOX_VE_API_TOKEN is written).
func ProxmoxCSIToken(token string) (id, secret string, err error) {
	id, secret, ok := strings.Cut(strings.TrimSpace(token), "=")
	if !ok || !strings.Contains(id, "!") || secret == "" {
		return "", "", fmt.Errorf("%s doesn't look like an API token: the token ID (like kubernetes-csi@pve!csi), '=', then its secret", config.ProxmoxCSITokenEnv)
	}
	return id, secret, nil
}

// ebs is the EBS CSI driver: its controller on the control planes, which alone carry its IAM
// role; every volume tagged with the cluster, so the leftover check after a delete sees it.
func (o Options) ebs() component {
	c := o.storage(config.CSIAWSEBS, fileVersion(o.Kubernetes.Storage.AWSEBS), EBSVersion)
	values := map[string]any{"controller": map[string]any{
		"extraVolumeTags": map[string]any{cloud.TagCluster: o.Cluster},
		"nodeSelector":    map[string]any{controlPlane: ""},
		"tolerations": []any{
			map[string]any{"key": controlPlane, "operator": "Exists", "effect": "NoSchedule"},
			map[string]any{"key": "CriticalAddonsOnly", "operator": "Exists"},
		},
	}}
	c.steps = []step{
		o.helmStep("aws-ebs-csi-driver", "aws-ebs-csi-driver", ebsCharts, c.version, "kube-system", values),
		o.apply(fmt.Sprintf(`apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: %s
provisioner: ebs.csi.aws.com
parameters: {type: gp3, encrypted: "true"}
reclaimPolicy: Delete
volumeBindingMode: WaitForFirstConsumer
allowVolumeExpansion: true
`, StorageClasses[config.CSIAWSEBS])),
	}
	return c
}

//go:embed gcp-pd.yaml.tmpl
var gcpPDManifest string

// gcpPD is the GCE PD CSI driver: its manifests (the tested release's, with the file's driver
// image), the controller on the control planes, which alone carry its service account; every
// disk labeled with the cluster, so the leftover check after a delete sees it.
func (o Options) gcpPD() component {
	c := o.storage(config.CSIGCPPD, fileVersion(o.Kubernetes.Storage.GCPPD), GCPPDVersion)
	c.owns = "gce-pd-csi-driver"
	c.deployment = "gce-pd-csi-driver/csi-gce-pd-controller" // its first container is the driver
	var manifest strings.Builder
	t := template.Must(template.New("gcp-pd").Option("missingkey=error").Parse(gcpPDManifest))
	if err := t.Execute(&manifest, map[string]string{"Version": c.version, "Cluster": o.Cluster}); err != nil {
		panic(err) // the template is embedded and its keys are given
	}
	rollout := func(what string) step {
		return step{program: "kubectl", args: []string{"rollout", "status", what, "--namespace", "gce-pd-csi-driver", "--kubeconfig", o.Kubeconfig, "--timeout", chartTimeout}}
	}
	c.steps = []step{
		o.apply(manifest.String()),
		rollout("deployment/csi-gce-pd-controller"),
		rollout("daemonset/csi-gce-pd-node"),
		o.apply(fmt.Sprintf(`apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: %s
provisioner: pd.csi.storage.gke.io
parameters: {type: pd-balanced}
reclaimPolicy: Delete
volumeBindingMode: WaitForFirstConsumer
allowVolumeExpansion: true
`, StorageClasses[config.CSIGCPPD])),
	}
	return c
}

// azureDisk is the Azure Disk CSI driver: its cloud config (IDs only: the managed identity the
// control planes carry, the resource group its disks go in), then the chart, its controller on
// the control planes, then a StorageClass whose disks carry the cluster's tag. The IDs come
// from the machines' module once they exist (Options.CloudStorage); a dry run shows where they go.
func (o Options) azureDisk() component {
	c := o.storage(config.CSIAzureDisk, fileVersion(o.Kubernetes.Storage.AzureDisk), AzureDiskVersion)
	s := o.CloudStorage
	cloudConfig, _ := json.Marshal(map[string]any{
		"cloud": "AzurePublicCloud", "tenantId": s["tenant_id"], "subscriptionId": s["subscription"],
		"resourceGroup": s["resource_group"], "location": s["location"], "vmType": "standard",
		"useManagedIdentityExtension": true, "userAssignedIdentityID": s["identity"], "useInstanceMetadata": true,
	})
	secret := map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata":   map[string]any{"name": "azure-cloud-provider", "namespace": "kube-system"},
		"stringData": map[string]any{"cloud-config": string(cloudConfig)},
	}
	secretDoc, _ := json.Marshal(secret)
	values := map[string]any{
		// One replica: it uses the host's network, so two on one control plane would share ports.
		"controller": map[string]any{"runOnControlPlane": true, "replicas": 1},
		"windows":    map[string]any{"enabled": false},
	}
	c.preRemove = nil
	c.postRemove = []step{{program: "kubectl", args: []string{"delete", "secret", "azure-cloud-provider", "--namespace", "kube-system", "--ignore-not-found", "--kubeconfig", o.Kubeconfig}}}
	c.steps = []step{
		o.apply(string(secretDoc)),
		o.helmStep("azuredisk-csi-driver", "azuredisk-csi-driver", azureDiskCharts, c.version, "kube-system", values),
		o.apply(fmt.Sprintf(`apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: %s
provisioner: disk.csi.azure.com
parameters: {skuName: StandardSSD_LRS, tags: "%s=%s"}
reclaimPolicy: Delete
volumeBindingMode: WaitForFirstConsumer
allowVolumeExpansion: true
`, StorageClasses[config.CSIAzureDisk], cloud.TagCluster, o.Cluster)),
	}
	return c
}

// proxmoxCSI is the Proxmox CSI plugin: the nodes labeled with their region and zone, then the
// chart with its token and StorageClass.
func (o Options) proxmoxCSI() component {
	p := o.Proxmox
	c := o.storage(config.CSIProxmox, fileVersion(o.Kubernetes.Storage.Proxmox), ProxmoxCSIVersion)
	c.owns = "csi-proxmox"
	id, secret, _ := ProxmoxCSIToken(o.Secrets[config.ProxmoxCSITokenEnv]) // checked when it was read
	ui.RegisterSecret(secret)
	storage := p.Storage
	if storage == "" {
		storage = "local-lvm"
	}
	values := map[string]any{
		"config": map[string]any{"clusters": []any{map[string]any{
			"url": strings.TrimRight(p.URL, "/") + "/api2/json", "insecure": p.Insecure,
			"token_id": id, "token_secret": secret, "region": o.Cluster,
		}}},
		"storageClass": []any{map[string]any{
			"name": StorageClasses[config.CSIProxmox], "storage": storage, "reclaimPolicy": "Delete", "fstype": "ext4",
		}},
	}
	// Its region and zone labels go with it.
	c.postRemove = []step{{program: "kubectl", args: []string{"label", "nodes", "--all", "--kubeconfig", o.Kubeconfig,
		"topology.kubernetes.io/region-", "topology.kubernetes.io/zone-"}}}
	// The nodes by name, so a node added later makes this a change, and gets its labels.
	nodes := []string{"--all"}
	if len(o.Nodes) > 0 {
		nodes = nil
		for _, n := range o.Nodes {
			nodes = append(nodes, n.Name)
		}
	}
	c.steps = []step{
		{program: "kubectl", args: append(append([]string{"label", "nodes"}, nodes...), "--overwrite", "--kubeconfig", o.Kubeconfig,
			"topology.kubernetes.io/region="+o.Cluster, "topology.kubernetes.io/zone="+p.Node)},
		o.apply(`apiVersion: v1
kind: Namespace
metadata:
  name: csi-proxmox
  labels:
    pod-security.kubernetes.io/enforce: privileged
    pod-security.kubernetes.io/audit: privileged
    pod-security.kubernetes.io/warn: privileged
`),
		{program: "helm", args: []string{"upgrade", "--install", "proxmox-csi-plugin", proxmoxCSIChart, "--version", c.version,
			"--namespace", "csi-proxmox", "--kubeconfig", o.Kubeconfig, "--wait", "--timeout", chartTimeout, "--values", "{values}"}, values: values},
	}
	return c
}
