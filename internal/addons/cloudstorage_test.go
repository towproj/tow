package addons

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/ui"
)

// EBS: the controller on the control planes (they alone carry its IAM role), every volume
// tagged with the cluster, a gp3 class.
func TestEBS(t *testing.T) {
	o := Options{Cluster: "lab", Kubeconfig: "/state/kc", Kubernetes: config.Kubernetes{Distribution: config.DistributionKubeadm,
		Storage: config.Storage{CSI: []string{config.CSIAWSEBS}, DefaultClass: config.CSIAWSEBS}}}
	first, _ := o.components()
	if len(first) != 2 || first[0].name != "aws-ebs" || first[1].name != "default StorageClass aws-ebs" {
		t.Fatalf("%+v", first)
	}
	chart, class := first[0].steps[0], first[0].steps[1]
	if !strings.Contains(strings.Join(chart.args, " "), "upgrade --install aws-ebs-csi-driver aws-ebs-csi-driver --repo https://kubernetes-sigs.github.io/aws-ebs-csi-driver --version 2.68.0 --namespace kube-system") {
		t.Errorf("args %v", chart.args)
	}
	v := chart.values
	if at(v, "controller", "extraVolumeTags", "tow-cluster") != "lab" || at(v, "controller", "nodeSelector", "node-role.kubernetes.io/control-plane") != "" {
		t.Errorf("values %v", v)
	}
	for _, want := range []string{"name: aws-ebs", "provisioner: ebs.csi.aws.com", "type: gp3", `encrypted: "true"`, "WaitForFirstConsumer"} {
		if !strings.Contains(class.stdin, want) {
			t.Errorf("class lacks %q:\n%s", want, class.stdin)
		}
	}
}

// GCE PD: the release's manifests with the file's driver image and the cluster's disk label,
// rolled out, then a pd-balanced class.
func TestGCPPD(t *testing.T) {
	o := Options{Cluster: "lab", Kubeconfig: "/state/kc", Kubernetes: config.Kubernetes{Distribution: config.DistributionKubeadm,
		Storage: config.Storage{CSI: []string{config.CSIGCPPD}, GCPPD: &config.Pinned{Version: "1.28.2"}}}}
	c := o.gcpPD()
	if c.version != "1.28.2" || c.deployment != "gce-pd-csi-driver/csi-gce-pd-controller" || len(c.steps) != 4 {
		t.Fatalf("%+v", c)
	}
	m := c.steps[0].stdin
	for _, want := range []string{"kind: Namespace", "gcp-compute-persistent-disk-csi-driver:v1.28.2", "--extra-labels=tow-cluster=lab",
		"node-role.kubernetes.io/control-plane: \"\"", "name: csi-gce-pd-node", "kind: CSIDriver"} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %q", want)
		}
	}
	for _, gone := range []string{"{{", "win", "--enable-data-cache\n", "DoesNotExist", "--run-taint-controller", "containerPort: 8081"} {
		if strings.Contains(m, gone) {
			t.Errorf("manifest still has %q", gone)
		}
	}
	if class := c.steps[3].stdin; !strings.Contains(class, "provisioner: pd.csi.storage.gke.io") || !strings.Contains(class, "type: pd-balanced") {
		t.Errorf("class:\n%s", class)
	}
}

// Azure Disk: its cloud config from the machines' module (IDs only), the chart with its
// controller on the control planes, then a class tagging disks with the cluster.
func TestAzureDisk(t *testing.T) {
	o := Options{Cluster: "lab", Kubeconfig: "/state/kc", Kubernetes: config.Kubernetes{Distribution: config.DistributionK3s,
		Storage: config.Storage{CSI: []string{config.CSIAzureDisk}}},
		CloudStorage: map[string]string{"tenant_id": "t-1", "subscription": "s-1", "resource_group": "tow-lab", "location": "westeurope", "identity": "c-1"}}
	c := o.azureDisk()
	if len(c.steps) != 3 {
		t.Fatalf("%+v", c)
	}
	var secret struct {
		Metadata   struct{ Name, Namespace string }
		StringData map[string]string `json:"stringData"`
	}
	if err := json.Unmarshal([]byte(c.steps[0].stdin), &secret); err != nil {
		t.Fatal(err)
	}
	var cc map[string]any
	if err := json.Unmarshal([]byte(secret.StringData["cloud-config"]), &cc); err != nil || secret.Metadata.Name != "azure-cloud-provider" || secret.Metadata.Namespace != "kube-system" {
		t.Fatalf("secret %+v, %v", secret, err)
	}
	for k, want := range map[string]any{"tenantId": "t-1", "subscriptionId": "s-1", "resourceGroup": "tow-lab", "location": "westeurope",
		"useManagedIdentityExtension": true, "userAssignedIdentityID": "c-1", "vmType": "standard"} {
		if cc[k] != want {
			t.Errorf("cloud config %s = %v, want %v", k, cc[k], want)
		}
	}
	chart := c.steps[1]
	if !strings.Contains(strings.Join(chart.args, " "), "upgrade --install azuredisk-csi-driver azuredisk-csi-driver --repo "+azureDiskCharts+" --version 1.36.0 --namespace kube-system") {
		t.Errorf("args %v", chart.args)
	}
	if at(chart.values, "controller", "runOnControlPlane") != true || at(chart.values, "controller", "replicas") != 1 || at(chart.values, "windows", "enabled") != false {
		t.Errorf("values %v", chart.values)
	}
	if class := c.steps[2].stdin; !strings.Contains(class, "provisioner: disk.csi.azure.com") || !strings.Contains(class, `tags: "tow-cluster=lab"`) {
		t.Errorf("class:\n%s", class)
	}
}

// Proxmox CSI: the nodes labeled with region and zone, its privileged namespace, then its
// chart with its own token and a class on the file's storage. The token never shows.
func TestProxmoxCSI(t *testing.T) {
	o := Options{Cluster: "lab", Kubeconfig: "/state/kc",
		Kubernetes: config.Kubernetes{Distribution: config.DistributionK3s, Storage: config.Storage{CSI: []string{config.CSIProxmox}}},
		Proxmox:    &Proxmox{URL: "https://192.168.100.10:8006/", Node: "pve", Insecure: true},
		Secrets:    map[string]string{config.ProxmoxCSITokenEnv: "kubernetes-csi@pve!csi=0b7e2a54-aaaa-bbbb-cccc-444455556666"}}
	c := o.proxmoxCSI()
	if len(c.steps) != 3 {
		t.Fatalf("%+v", c.steps)
	}
	if got := strings.Join(c.steps[0].args, " "); !strings.Contains(got, "label nodes --all --overwrite") || !strings.Contains(got, "topology.kubernetes.io/region=lab topology.kubernetes.io/zone=pve") {
		t.Errorf("labels: %s", got)
	}
	if !strings.Contains(c.steps[1].stdin, "name: csi-proxmox") || !strings.Contains(c.steps[1].stdin, "enforce: privileged") {
		t.Errorf("namespace: %s", c.steps[1].stdin)
	}
	chart := c.steps[2]
	if !strings.Contains(strings.Join(chart.args, " "), "upgrade --install proxmox-csi-plugin oci://ghcr.io/sergelogvinov/charts/proxmox-csi-plugin --version 0.5.15 --namespace csi-proxmox") {
		t.Errorf("args %v", chart.args)
	}
	cl := at(chart.values, "config", "clusters").([]any)[0].(map[string]any)
	if cl["url"] != "https://192.168.100.10:8006/api2/json" || cl["insecure"] != true || cl["token_id"] != "kubernetes-csi@pve!csi" || cl["region"] != "lab" {
		t.Errorf("cluster %v", cl)
	}
	sc := chart.values["storageClass"].([]any)[0].(map[string]any)
	if sc["name"] != "proxmox" || sc["storage"] != "local-lvm" || sc["reclaimPolicy"] != "Delete" {
		t.Errorf("class %v", sc)
	}
	if strings.Contains(strings.Join(describe("proxmox-csi", c.steps), "\n"), "0b7e2a54") || strings.Contains(ui.Redact("x 0b7e2a54-aaaa-bbbb-cccc-444455556666 y"), "0b7e2a54") {
		t.Error("the token secret shows")
	}

	if _, _, err := ProxmoxCSIToken("just-a-secret"); err == nil || !strings.Contains(err.Error(), "PROXMOX_CSI_TOKEN doesn't look like an API token") {
		t.Errorf("err = %v", err)
	}
	if got := strings.Join(Secrets(o.Kubernetes), ","); got != "PROXMOX_CSI_TOKEN" {
		t.Errorf("secrets %s", got)
	}
}
