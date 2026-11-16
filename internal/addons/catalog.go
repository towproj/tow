package addons

import (
	"cmp"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/ui"
)

// Sources (read 2026-11-11), followed here:
//   - local-path-provisioner: its manifest at the release tag, deploy/local-path-storage.yaml
//     (namespace local-path-storage, StorageClass local-path, not the default)
//     https://github.com/rancher/local-path-provisioner
//   - OpenEBS 4.6, the umbrella chart from openebs.github.io/openebs: it turns on LVM, ZFS,
//     hostpath, Mayastor, Loki and Alloy by default; only hostpath stays on. StorageClass
//     openebs-hostpath  https://openebs.io/docs/quickstart-guide/installation
//   - Longhorn 1.13 with Helm (charts.longhorn.io): persistence.defaultClass,
//     persistence.defaultClassReplicaCount, defaultSettings.defaultReplicaCount
//     https://longhorn.io/docs/1.13.0/deploy/install/install-with-helm/
//   - csi-driver-nfs v4.13.6 into kube-system; the StorageClass as its
//     deploy/example/storageclass-nfs.yaml (nfsvers=4.1). Its image carries nfs-common.
//   - csi-driver-smb v1.20.4 into kube-system; the credentials Secret and the StorageClass as its
//     deploy/example/storageclass-smb.yaml ("noserverino # required to prevent data
//     corruption"). Its image carries cifs-utils.
//   - metrics-server, its chart 3.14.0 (metrics-server 0.9.0)
//     https://github.com/kubernetes-sigs/metrics-server
//   - kubeadm: kubelets get serving certificates the cluster signs (serverTLSBootstrap), which
//     "will have to be approved"; kubelet-csr-approver is the controller the page names
//     https://kubernetes.io/docs/tasks/administer-cluster/kubeadm/kubeadm-certs/#kubelet-serving-certs
//   - kubelet-csr-approver 1.2.15: providerRegex, providerIpPrefixes, bypassDnsResolution
//     https://github.com/postfinance/kubelet-csr-approver

// Tested releases, installed when the file pins none. A chart's version is the chart's own
// (metrics-server's chart 3.14.0 carries metrics-server 0.9.0).
const (
	LocalPathVersion     = "0.0.37"
	OpenEBSVersion       = "4.6.2"
	LonghornVersion      = "1.13.0"
	NFSVersion           = "4.13.6"
	SMBVersion           = "1.20.4"
	MetricsServerVersion = "3.14.0"
	CSRApproverVersion   = "1.2.15"

	openebsCharts       = "https://openebs.github.io/openebs"
	longhornCharts      = "https://charts.longhorn.io"
	nfsCharts           = "https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/master/charts"
	smbCharts           = "https://raw.githubusercontent.com/kubernetes-csi/csi-driver-smb/master/charts"
	metricsServerCharts = "https://kubernetes-sigs.github.io/metrics-server/"
	csrApproverCharts   = "https://postfinance.github.io/kubelet-csr-approver"

	smbSecret = "smb-credentials"

	defaultClassKey = "storageclass.kubernetes.io/is-default-class"
)

// VolumeDrivers are what the volumes of each storage driver tow installs name: the CSI
// driver, or the provisioner for those that aren't CSI.
var VolumeDrivers = map[string]string{
	config.CSILocalPath: "rancher.io/local-path",
	config.CSIOpenEBS:   "openebs.io/local",
	config.CSILonghorn:  "driver.longhorn.io",
	config.CSINFS:       "nfs.csi.k8s.io",
	config.CSISMB:       "smb.csi.k8s.io",
	config.CSIAWSEBS:    "ebs.csi.aws.com",
	config.CSIProxmox:   "csi.proxmox.sinextra.dev",
	config.CSIGCPPD:     "pd.csi.storage.gke.io",
	config.CSIAzureDisk: "disk.csi.azure.com",
}

// StorageClasses are the class each storage driver tow installs offers: the file's
// defaultClass names the driver.
var StorageClasses = map[string]string{
	config.CSILocalPath: "local-path",
	config.CSIOpenEBS:   "openebs-hostpath",
	config.CSILonghorn:  "longhorn",
	config.CSINFS:       "nfs",
	config.CSISMB:       "smb",
	config.CSIAWSEBS:    "aws-ebs",
	config.CSIProxmox:   "proxmox",
	config.CSIGCPPD:     "gcp-pd",
	config.CSIAzureDisk: "azure-disk",
}

// CloudVolumes are the storage drivers whose volumes live outside the machines (an EBS
// volume, a Proxmox disk, a persistent disk): they outlive a cluster's machines unless
// deleted with it. CloudVolumeDrivers are their CSI driver names, which their
// PersistentVolumes carry.
var (
	CloudVolumes       = []string{config.CSIAWSEBS, config.CSIProxmox, config.CSIGCPPD, config.CSIAzureDisk}
	CloudVolumeDrivers = []string{"ebs.csi.aws.com", "csi.proxmox.sinextra.dev", "pd.csi.storage.gke.io", "disk.csi.azure.com"}
)

// LocalVolumes keep their data on the machines' own disks. On the user's own machines that
// data outlives the cluster, or goes with it (k3s's uninstaller deletes its local-path
// folder): delete treats them as the cloud volumes there. LocalVolumeDrivers are what their
// PersistentVolumes name: the CSI driver, or the provisioner for those that aren't CSI.
var (
	LocalVolumes       = []string{config.CSILocalPath, config.CSIOpenEBS, config.CSILonghorn}
	LocalVolumeDrivers = []string{"rancher.io/local-path", "openebs.io/local", "driver.longhorn.io"}
)

// component is one add-on, as the commands that install it, and what a running cluster
// needs to change it or turn it off (builtins.go).
type component struct {
	name, version string
	steps         []step

	pin, tested string // the file's version ("" = none), and the one tow was tested with
	role        string // roleStorage, roleDefaultClass, roleServiceLB, roleIngress; "" otherwise
	csi         string // storage: the file's name for it
	class       string // the StorageClass it offers (or marks the default), the IngressClass it serves
	deployment  string // namespace/name whose image tag is its version, when no Helm release says it
	owns        string // the namespace only it uses, deleted with it
	// preRemove and postRemove come before and after what turning it off always does:
	// deleting the objects it applied, uninstalling its release or deleting its manifest.
	preRemove, postRemove []step
}

// Roles of the components a running cluster can turn off, which decide what's checked first.
const (
	roleStorage      = "storage"
	roleDefaultClass = "default StorageClass"
	roleServiceLB    = "service load balancer"
	roleIngress      = "ingress"
)

// versioned starts a component: its version is the file's, else the one that runs (a running
// cluster keeps it), else the one tow was tested with.
func (o Options) versioned(name, file, tested string) component {
	file = strings.TrimPrefix(file, "v")
	return component{name: name, version: cmp.Or(file, o.Running[name], tested), pin: file, tested: tested}
}

// fileVersion is a pinned component's version in the file, "" when it names none.
func fileVersion(p *config.Pinned) string {
	if p == nil {
		return ""
	}
	return p.Version
}

// Bundled reports whether the distribution brings a component itself: k3s (and k3d, which
// runs k3s) ships local-path, traefik, servicelb and metrics-server.
func Bundled(distribution, component string) bool {
	if distribution != config.DistributionK3s && distribution != config.DistributionK3d {
		return false
	}
	switch component {
	case config.CSILocalPath, config.IngressTraefik, config.ServiceLBServiceLB, config.MetricsServerName:
		return true
	}
	return false
}

// components are the file's add-ons tow installs, in order: storage (and the default
// StorageClass), the service load balancer, metrics-server, then the manifests. Helm charts
// come between the last two.
func (o Options) components() (first, last []component) {
	k := o.Kubernetes
	d := k.Distribution
	s := k.Storage
	for _, csi := range s.CSI {
		if Bundled(d, csi) {
			continue
		}
		switch csi {
		case config.CSILocalPath:
			first = append(first, o.localPath())
		case config.CSIOpenEBS:
			first = append(first, o.openebs())
		case config.CSILonghorn:
			first = append(first, o.longhorn())
		case config.CSINFS:
			first = append(first, o.nfs())
		case config.CSISMB:
			first = append(first, o.smb())
		case config.CSIAWSEBS:
			first = append(first, o.ebs())
		case config.CSIProxmox:
			first = append(first, o.proxmoxCSI())
		case config.CSIGCPPD:
			first = append(first, o.gcpPD())
		case config.CSIAzureDisk:
			first = append(first, o.azureDisk())
		}
	}
	// The default StorageClass, unless k3s's local-path already is (config checks it's asked).
	if class, ok := StorageClasses[s.DefaultClass]; ok && !Bundled(d, s.DefaultClass) {
		annotate := func(value string) step {
			return step{program: "kubectl", args: []string{"annotate", "storageclass", class, defaultClassKey + value, "--overwrite", "--kubeconfig", o.Kubeconfig}}
		}
		first = append(first, component{name: "default StorageClass " + class, role: roleDefaultClass, class: class,
			steps: []step{annotate("=true")}, preRemove: []step{annotate("-")}})
	}
	first = append(first, o.serviceLB()...)
	if slices.Contains(k.Ingress.Controllers, config.IngressTraefik) && !Bundled(d, config.IngressTraefik) {
		first = append(first, o.traefik())
	}
	if m := k.MetricsServer.Enabled; m != nil && *m && !Bundled(d, config.MetricsServerName) {
		if d == config.DistributionKubeadm {
			first = append(first, o.csrApprover())
		}
		first = append(first, o.metricsServer())
	}
	for _, m := range k.Addons.Manifests {
		last = append(last, o.manifest(m))
	}
	return first, last
}

// helmStep installs or upgrades a chart from an https repository and waits for it.
func (o Options) helmStep(release, chart, repo, version, namespace string, values map[string]any) step {
	args := []string{"upgrade", "--install", release, chart, "--repo", repo, "--version", version,
		"--namespace", namespace, "--create-namespace", "--kubeconfig", o.Kubeconfig, "--wait", "--timeout", chartTimeout}
	if values != nil {
		args = append(args, "--values", "{values}")
	}
	return step{program: "helm", args: args, values: values}
}

// apply is kubectl apply of what stdin holds.
func (o Options) apply(stdin string) step {
	return step{program: "kubectl", args: []string{"apply", "--kubeconfig", o.Kubeconfig, "-f", "-"}, stdin: stdin}
}

// storage starts a storage driver's component.
func (o Options) storage(csi, file, tested string) component {
	c := o.versioned(csi, file, tested)
	c.role, c.csi, c.class = roleStorage, csi, StorageClasses[csi]
	return c
}

func (o Options) localPath() component {
	c := o.storage(config.CSILocalPath, fileVersion(o.Kubernetes.Storage.LocalPath), LocalPathVersion)
	url := fmt.Sprintf("https://raw.githubusercontent.com/rancher/local-path-provisioner/v%s/deploy/local-path-storage.yaml", c.version)
	c.deployment = "local-path-storage/local-path-provisioner" // its manifest holds the namespace
	c.steps = []step{
		{program: "kubectl", args: []string{"apply", "--server-side", "--kubeconfig", o.Kubeconfig, "-f", url}},
		{program: "kubectl", args: []string{"rollout", "status", "deployment/local-path-provisioner", "--namespace", "local-path-storage", "--kubeconfig", o.Kubeconfig, "--timeout", chartTimeout}},
	}
	return c
}

func (o Options) openebs() component {
	c := o.storage(config.CSIOpenEBS, fileVersion(o.Kubernetes.Storage.OpenEBS), OpenEBSVersion)
	c.owns = "openebs"
	values := map[string]any{
		"engines": map[string]any{
			"local":      map[string]any{"lvm": map[string]any{"enabled": false}, "zfs": map[string]any{"enabled": false}},
			"replicated": map[string]any{"mayastor": map[string]any{"enabled": false}},
		},
		"loki":  map[string]any{"enabled": false},
		"alloy": map[string]any{"enabled": false},
	}
	c.steps = []step{o.helmStep("openebs", "openebs", openebsCharts, c.version, "openebs", values)}
	return c
}

func (o Options) longhorn() component {
	l := o.Kubernetes.Storage.Longhorn
	file := ""
	if l != nil {
		file = l.Version
	}
	c := o.storage(config.CSILonghorn, file, LonghornVersion)
	c.owns = "longhorn-system"
	persistence := map[string]any{"defaultClass": false} // tow marks the file's default
	values := map[string]any{"persistence": persistence}
	if l != nil && l.Replicas != nil {
		persistence["defaultClassReplicaCount"] = *l.Replicas
		values["defaultSettings"] = map[string]any{"defaultReplicaCount": *l.Replicas}
	}
	c.steps = []step{o.helmStep("longhorn", "longhorn", longhornCharts, c.version, "longhorn-system", values)}
	// Longhorn refuses to be uninstalled until this setting says so (its docs, "Uninstall").
	c.preRemove = []step{{program: "kubectl", args: []string{"patch", "settings.longhorn.io", "deleting-confirmation-flag", "--namespace", "longhorn-system",
		"--type", "merge", "--patch", `{"value": "true"}`, "--kubeconfig", o.Kubeconfig}}}
	return c
}

func (o Options) nfs() component {
	n := o.Kubernetes.Storage.NFS
	c := o.storage(config.CSINFS, n.Version, NFSVersion)
	c.steps = []step{
		o.helmStep("csi-driver-nfs", "csi-driver-nfs", nfsCharts, c.version, "kube-system", nil),
		o.apply(fmt.Sprintf(`apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: %s
provisioner: nfs.csi.k8s.io
parameters:
  server: %q
  share: %q
reclaimPolicy: Delete
volumeBindingMode: Immediate
allowVolumeExpansion: true
mountOptions: [nfsvers=4.1]
`, StorageClasses[config.CSINFS], n.Server, n.Path)),
	}
	return c
}

func (o Options) smb() component {
	m := o.Kubernetes.Storage.SMB
	c := o.storage(config.CSISMB, m.Version, SMBVersion)
	user, password := o.Secrets[m.UsernameEnv], o.Secrets[m.PasswordEnv]
	ui.RegisterSecret(password)
	c.steps = []step{
		o.helmStep("csi-driver-smb", "csi-driver-smb", smbCharts, c.version, "kube-system", nil),
		o.apply(fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %[1]s
  namespace: kube-system
type: Opaque
stringData:
  username: %[2]q
  password: %[3]q
---
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: %[4]s
provisioner: smb.csi.k8s.io
parameters:
  source: %[5]q
  csi.storage.k8s.io/provisioner-secret-name: %[1]s
  csi.storage.k8s.io/provisioner-secret-namespace: kube-system
  csi.storage.k8s.io/node-stage-secret-name: %[1]s
  csi.storage.k8s.io/node-stage-secret-namespace: kube-system
reclaimPolicy: Delete
volumeBindingMode: Immediate
allowVolumeExpansion: true
mountOptions: [dir_mode=0777, file_mode=0777, uid=1001, gid=1001, noperm, mfsymlinks, cache=strict, noserverino]
`, smbSecret, user, password, StorageClasses[config.CSISMB], m.Source)),
	}
	return c
}

// csrApprover approves the serving certificates kubeadm's kubelets ask for: only for the
// cluster's own node names and addresses (they aren't in DNS, so that check is skipped).
func (o Options) csrApprover() component {
	var names, prefixes []string
	for _, n := range o.Nodes {
		names = append(names, regexp.QuoteMeta(n.Name))
		for _, ip := range []string{n.PrivateIP, n.Host} {
			if a, err := netip.ParseAddr(ip); err == nil {
				prefixes = append(prefixes, netip.PrefixFrom(a, a.BitLen()).String())
			}
		}
	}
	values := map[string]any{
		"providerRegex":       "^(" + strings.Join(names, "|") + ")$",
		"providerIpPrefixes":  prefixes,
		"bypassDnsResolution": true,
	}
	if len(o.Nodes) == 1 {
		values["replicas"] = 1
	}
	c := o.versioned("kubelet-csr-approver", "", CSRApproverVersion)
	c.steps = []step{o.helmStep("kubelet-csr-approver", "kubelet-csr-approver", csrApproverCharts, c.version, "kube-system", values)}
	return c
}

func (o Options) metricsServer() component {
	c := o.versioned(config.MetricsServerName, o.Kubernetes.MetricsServer.Version, MetricsServerVersion)
	c.steps = []step{o.helmStep("metrics-server", "metrics-server", metricsServerCharts, c.version, "kube-system", nil)}
	return c
}

// manifest applies one of the file's manifests: its objects that name no namespace go into
// its own (created first), the others where they say.
func (o Options) manifest(m config.Manifest) component {
	ns := m.Target()
	path := m.Path
	args := []string{"apply", "--server-side", "--field-manager", "tow", "--kubeconfig", "{kubeconfig}", "-f", path}
	if !strings.Contains(m.Path, "://") {
		if o.ManifestPath != nil {
			path = o.ManifestPath(m.Path)
		}
		args = []string{"apply", "--server-side", "--field-manager", "tow", "--kubeconfig", "{kubeconfig}", "--recursive", "-f", path}
	}
	// The namespace has a field manager of its own: when the manifest declares the same
	// namespace, applying it then can't take over and drop tow's label.
	return component{name: "manifest " + m.Path, steps: []step{
		{program: "kubectl", args: []string{"apply", "--server-side", "--field-manager", "tow-namespace", "--kubeconfig", o.Kubeconfig, "-f", "-"},
			stdin: fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n  labels: {app.kubernetes.io/managed-by: tow}\n", ns)},
		{program: "kubectl", args: args, namespaced: ns},
	}}
}

// NeedsKubectl and NeedsHelm report whether the file's add-ons take kubectl (local-path's
// manifest, a StorageClass, the default class, the manifests) or helm (the charts).
func (o Options) NeedsKubectl() bool { return o.uses("kubectl") }
func (o Options) NeedsHelm() bool    { return len(o.Charts) > 0 || o.uses("helm") }

func (o Options) uses(program string) bool {
	first, last := o.components()
	for _, c := range append(first, last...) {
		for _, s := range c.steps {
			if s.program == program {
				return true
			}
		}
	}
	return false
}

// Empty reports whether there's nothing to install: no add-on, chart or manifest.
func (o Options) Empty() bool {
	first, last := o.components()
	return len(first)+len(last)+len(o.Charts) == 0
}
