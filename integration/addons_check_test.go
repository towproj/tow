package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/towproj/tow/internal/addons"
	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/state"
	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

// networked storage keeps a volume reachable from another node. A Proxmox disk moves to
// another VM of the same server; an EBS volume can't leave its availability zone, which the
// nodes don't share.
var networked = []string{config.CSILonghorn, config.CSINFS, config.CSISMB, config.CSIProxmox}

// untainted counts the nodes the check's pods can run on: they tolerate no taint.
func untainted(nodes []corev1.Node) int {
	n := 0
	for _, node := range nodes {
		if !slices.ContainsFunc(node.Spec.Taints, func(t corev1.Taint) bool { return t.Effect != corev1.TaintEffectPreferNoSchedule }) {
			n++
		}
	}
	return n
}

// checkStorage proves each storage driver tow installed works: its StorageClass exists,
// the file's default is the only default, and a volume keeps what a pod wrote for the next
// pod (on another node, for networked storage). An NFS or SMB server the file names as an
// in-cluster Service is started first from the drivers' own example servers.
func checkStorage(t *testing.T, ctx context.Context, cs kubernetes.Interface, cfg *config.Config, nodes []corev1.Node) {
	t.Helper()
	s := cfg.Spec.Kubernetes.Storage
	var classes []string
	for _, csi := range s.CSI {
		if class, ok := addons.StorageClasses[csi]; ok {
			classes = append(classes, csi)
			if _, err := cs.StorageV1().StorageClasses().Get(ctx, class, metav1.GetOptions{}); err != nil {
				t.Errorf("StorageClass %s for %s: %v", class, csi, err)
			}
		}
	}
	if len(classes) == 0 {
		return
	}
	if want, ok := addons.StorageClasses[s.DefaultClass]; ok {
		list, err := cs.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var defaults []string
		for _, sc := range list.Items {
			if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
				defaults = append(defaults, sc.Name)
			}
		}
		if !slices.Equal(defaults, []string{want}) {
			t.Errorf("default StorageClasses %v, want only %s", defaults, want)
		}
	}

	ns := "tow-check-storage"
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
	}()
	if slices.Contains(s.CSI, config.CSINFS) {
		startServer(t, ctx, cs, s.NFS.Server, nfsServer)
	}
	if slices.Contains(s.CSI, config.CSISMB) {
		startServer(t, ctx, cs, strings.SplitN(strings.TrimPrefix(s.SMB.Source, "//"), "/", 2)[0], smbServer(os.Getenv(s.SMB.UsernameEnv), os.Getenv(s.SMB.PasswordEnv)))
	}
	for _, csi := range classes {
		t.Run("volume "+csi, func(t *testing.T) {
			checkVolume(t, ctx, cs, ns, csi, addons.StorageClasses[csi], slices.Contains(networked, csi) && untainted(nodes) > 1)
		})
	}
}

// keepCloudVolume leaves a volume of the cluster's cloud storage (EBS, a Proxmox disk) in use,
// then proves a delete without -v refuses, naming it, and changes nothing. The caller's
// delete -v must then remove it with the cluster (on AWS, the leftover check proves it).
func keepCloudVolume(t *testing.T, ctx context.Context, c clusterCheck, del func() error) {
	t.Helper()
	var csi string
	for _, s := range c.cfg.Spec.Kubernetes.Storage.CSI {
		if slices.Contains(addons.CloudVolumes, s) {
			csi = s
		}
	}
	if csi == "" {
		return
	}
	cs := clientset(t, c.st)
	ns := "tow-check-keep"
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	class := addons.StorageClasses[csi]
	if _, err := cs.CoreV1().PersistentVolumeClaims(ns).Create(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "kept"},
		Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &class, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Pods(ns).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "user"}, Spec: corev1.PodSpec{
		Containers: []corev1.Container{{Name: "c", Image: agnhost, Args: []string{"pause"}, VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}}},
		Volumes:    []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "kept"}}}},
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	var volume string
	eventuallyWithin(t, ctx, 5*time.Minute, "a "+class+" volume in use", func() bool {
		p, err := cs.CoreV1().Pods(ns).Get(ctx, "user", metav1.GetOptions{})
		pvc, err2 := cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, "kept", metav1.GetOptions{})
		if err != nil || err2 != nil || p.Status.Phase != corev1.PodRunning {
			return false
		}
		volume = pvc.Spec.VolumeName
		return true
	})
	err := del()
	if err == nil || !strings.Contains(err.Error(), volume) || !strings.Contains(err.Error(), "-v") || !state.Exists(c.st.Name) {
		t.Fatalf("delete without -v must refuse, naming %s: %v", volume, err)
	}
	t.Logf("delete without -v refused, naming %s", volume)
}

// labSecrets gives the SMB share's credential variables a throwaway value when they're
// unset: the lab's share is the test's own server (startServer), made with the same ones.
func labSecrets(t *testing.T, cfg *config.Config) {
	t.Helper()
	m := cfg.Spec.Kubernetes.Storage.SMB
	if m == nil || !slices.Contains(cfg.Spec.Kubernetes.Storage.CSI, config.CSISMB) {
		return
	}
	for env, value := range map[string]string{m.UsernameEnv: "tow", m.PasswordEnv: fmt.Sprintf("lab-%d", time.Now().UnixNano())} {
		if os.Getenv(env) == "" {
			t.Setenv(env, value)
		}
	}
}

// checkVolume writes into a new volume from one pod and reads it from another.
func checkVolume(t *testing.T, ctx context.Context, cs kubernetes.Interface, ns, csi, class string, otherNode bool) {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "vol-" + csi},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &class,
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
		},
	}
	if _, err := cs.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// Where workloads run: a control plane with the control-plane taint has no storage node
	// plugin (Longhorn's), so the pods don't tolerate it.
	pod := func(name, script string, avoid string) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers: []corev1.Container{{Name: "c", Image: agnhost, Command: []string{"sh", "-c", script},
					VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}}},
				Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc.Name}}}},
			},
		}
		if avoid != "" {
			p.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{
					{Key: "metadata.name", Operator: corev1.NodeSelectorOpNotIn, Values: []string{avoid}}}}}}}}
		}
		return p
	}
	finished := func(name string) *corev1.Pod {
		var done *corev1.Pod
		defer func() { // on a timeout, say why the pod is stuck
			if done != nil {
				return
			}
			if p, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
				t.Logf("%s: %s on %q, conditions %+v", name, p.Status.Phase, p.Spec.NodeName, p.Status.Conditions)
			}
			if evs, err := cs.CoreV1().Events(ns).List(ctx, metav1.ListOptions{}); err == nil {
				for _, e := range evs.Items {
					t.Logf("event %s/%s %s: %s", e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Reason, e.Message)
				}
			}
		}()
		eventuallyWithin(t, ctx, 6*time.Minute, name+" finished", func() bool {
			p, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
			if err == nil && (p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed) {
				done = p
			}
			return done != nil
		})
		if done.Status.Phase != corev1.PodSucceeded {
			t.Fatalf("%s %s on %s", name, done.Status.Phase, done.Spec.NodeName)
		}
		return done
	}

	want := "written by the tow check on " + csi // goes between single quotes in sh
	writer := "write-" + csi
	if _, err := cs.CoreV1().Pods(ns).Create(ctx, pod(writer, "echo '"+want+"' > /data/check && sync", ""), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	wrote := finished(writer)
	if err := cs.CoreV1().Pods(ns).Delete(ctx, writer, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	avoid := ""
	if otherNode {
		avoid = wrote.Spec.NodeName
	}
	reader := "read-" + csi
	if _, err := cs.CoreV1().Pods(ns).Create(ctx, pod(reader, "cat /data/check", avoid), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	read := finished(reader)
	logs, err := cs.CoreV1().Pods(ns).GetLogs(reader, &corev1.PodLogOptions{}).DoRaw(ctx)
	if err != nil || strings.TrimSpace(string(logs)) != want {
		t.Errorf("%s read %q (%v), want %q", reader, logs, err, want)
	}
	t.Logf("%s: written on %s, read on %s", class, wrote.Spec.NodeName, read.Spec.NodeName)
}

// nfsServer and smbServer are the drivers' example servers (deploy/example/*-provisioner),
// for a lab with no NAS: the name and namespace come from the address the file gives.
func nfsServer(name, ns string) (*corev1.Service, *appsv1.Deployment, *corev1.Secret) {
	privileged := true
	labels := map[string]string{"app": name}
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: corev1.ServiceSpec{Selector: labels,
			Ports: []corev1.ServicePort{{Name: "tcp-2049", Port: 2049}, {Name: "udp-111", Port: 111, Protocol: corev1.ProtocolUDP}}}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
				Tolerations: []corev1.Toleration{{Key: controlPlaneTaint, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}},
				Containers: []corev1.Container{{Name: "nfs-server", Image: "itsthenetwork/nfs-server-alpine:latest",
					Env:             []corev1.EnvVar{{Name: "SHARED_DIRECTORY", Value: "/exports"}},
					SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
					VolumeMounts:    []corev1.VolumeMount{{Name: "data", MountPath: "/exports"}}}},
				Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
			}}}}, nil
}

func smbServer(user, password string) func(name, ns string) (*corev1.Service, *appsv1.Deployment, *corev1.Secret) {
	return func(name, ns string) (*corev1.Service, *appsv1.Deployment, *corev1.Secret) {
		labels := map[string]string{"app": name}
		return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: corev1.ServiceSpec{Selector: labels,
				Ports: []corev1.ServicePort{{Name: "smb", Port: 445}}}},
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
					Tolerations: []corev1.Toleration{{Key: controlPlaneTaint, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}},
					Containers: []corev1.Container{{Name: "smb-server", Image: "andyzhangx/samba:win-fix",
						Env: []corev1.EnvVar{{Name: "PERMISSIONS", Value: "0777"},
							{Name: "USERNAME", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: "username"}}},
							{Name: "PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: "password"}}}},
						Args:         []string{"-u", "$(USERNAME);$(PASSWORD)", "-s", "share;/smbshare/;yes;no;no;all;none", "-p"},
						VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/smbshare"}}}},
					Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
				}}}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, StringData: map[string]string{"username": user, "password": password}}
	}
}

// startServer starts a test server when host is an in-cluster Service name
// (<name>.<namespace>.svc.cluster.local); anything else is the user's own server.
func startServer(t *testing.T, ctx context.Context, cs kubernetes.Interface, host string, server func(name, ns string) (*corev1.Service, *appsv1.Deployment, *corev1.Secret)) {
	t.Helper()
	parts := strings.Split(host, ".")
	if len(parts) < 3 || parts[2] != "svc" {
		return
	}
	name, ns := parts[0], parts[1]
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatal(err)
	}
	svc, dep, secret := server(name, ns)
	if secret != nil {
		if _, err := cs.CoreV1().Secrets(ns).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cs.CoreV1().Services(ns).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventuallyWithin(t, ctx, 5*time.Minute, "test server "+host+" ready", func() bool {
		d, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		return err == nil && d.Status.ReadyReplicas == 1
	})
	t.Logf("test server %s started", host)
}

// checkServiceLB proves the service load balancer tow installed works: a LoadBalancer
// Service gets an address from the file's pool, and answers from this machine there.
func checkServiceLB(t *testing.T, ctx context.Context, cs kubernetes.Interface, cfg *config.Config) {
	t.Helper()
	lb := cfg.Spec.Kubernetes.ServiceLoadBalancer
	if !addons.ServiceLBInstalled(lb.Provider) {
		return
	}
	var pool []netip.Prefix
	for _, cidr := range prepare.PoolCIDRs(lb) {
		pool = append(pool, netip.MustParsePrefix(cidr))
	}
	ns := "tow-check-lb"
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
	}()
	labels := map[string]string{"app": "lb"}
	replicas := int32(1)
	if _, err := cs.AppsV1().Deployments(ns).Create(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "lb"}, Spec: appsv1.DeploymentSpec{
		Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: labels},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
			Tolerations: []corev1.Toleration{{Key: controlPlaneTaint, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}},
			Containers:  []corev1.Container{{Name: "netexec", Image: agnhost, Args: []string{"netexec", "--http-port=8080"}}},
		}},
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Services(ns).Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "lb"}, Spec: corev1.ServiceSpec{
		Type: corev1.ServiceTypeLoadBalancer, Selector: labels,
		Ports: []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(8080)}},
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	var ip string
	eventually(t, ctx, "the LoadBalancer Service getting an address from "+lb.Provider, func() bool {
		s, err := cs.CoreV1().Services(ns).Get(ctx, "lb", metav1.GetOptions{})
		if err == nil && len(s.Status.LoadBalancer.Ingress) > 0 {
			ip = s.Status.LoadBalancer.Ingress[0].IP
		}
		return ip != ""
	})
	addr, err := netip.ParseAddr(ip)
	if err != nil || !slices.ContainsFunc(pool, func(p netip.Prefix) bool { return p.Contains(addr) }) {
		t.Fatalf("the Service got %q, outside the pool %v", ip, pool)
	}
	client := http.Client{Timeout: 10 * time.Second}
	var last string
	eventually(t, ctx, "http://"+ip+"/hostname answering from this machine", func() bool {
		resp, err := client.Get("http://" + ip + "/hostname")
		if err != nil {
			last = err.Error()
			return false
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		last = string(body)
		return resp.StatusCode == http.StatusOK && strings.HasPrefix(last, "lb-")
	})
	t.Logf("LoadBalancer %s (%s) answered: %s", ip, lb.Provider, last)
}

// checkMetrics proves metrics-server answers for every node through the metrics API (on
// kubeadm, that the kubelets' serving certificates were signed and approved).
func checkMetrics(t *testing.T, ctx context.Context, cs kubernetes.Interface, nodes int) {
	t.Helper()
	var got int
	eventuallyWithin(t, ctx, 5*time.Minute, fmt.Sprintf("metrics for %d nodes", nodes), func() bool {
		raw, err := cs.CoreV1().RESTClient().Get().AbsPath("/apis/metrics.k8s.io/v1beta1/nodes").DoRaw(ctx)
		if err != nil {
			return false
		}
		var list struct {
			Items []json.RawMessage `json:"items"`
		}
		if json.Unmarshal(raw, &list) != nil {
			return false
		}
		got = len(list.Items)
		return got == nodes
	})
}

// checkManifests proves each manifest was applied: its namespace exists, marked as tow's,
// and the ConfigMaps a local manifest names that give no namespace are in it.
func checkManifests(t *testing.T, ctx context.Context, cs kubernetes.Interface, cfg *config.Config) {
	t.Helper()
	for _, m := range cfg.Spec.Kubernetes.Addons.Manifests {
		ns := m.Target()
		got, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
		if err != nil || got.Labels["app.kubernetes.io/managed-by"] != "tow" {
			t.Errorf("manifest %s: namespace %s: %v", m.Path, ns, err)
			continue
		}
		if strings.Contains(m.Path, "://") {
			continue
		}
		// A file, or every file of a folder.
		var data []byte
		filepath.WalkDir(cfg.ManifestPath(m.Path), func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if b, err := os.ReadFile(p); err == nil {
					if len(data) > 0 {
						data = append(data, "\n---\n"...)
					}
					data = append(data, b...)
				}
			}
			return nil
		})
		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		for {
			var obj struct {
				Kind     string `yaml:"kind"`
				Metadata struct {
					Name, Namespace string
				} `yaml:"metadata"`
			}
			if dec.Decode(&obj) != nil {
				break
			}
			if obj.Kind != "ConfigMap" {
				continue
			}
			where := obj.Metadata.Namespace
			if where == "" {
				where = ns
			}
			if _, err := cs.CoreV1().ConfigMaps(where).Get(ctx, obj.Metadata.Name, metav1.GetOptions{}); err != nil {
				t.Errorf("manifest %s: ConfigMap %s/%s: %v", m.Path, where, obj.Metadata.Name, err)
			}
		}
	}
}
