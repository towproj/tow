package kubeapi

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// What the plan checks before an add-on goes: LoadBalancer Services, the Ingresses a class
// serves (those naming none when it's the default), and a Deployment's image.
func TestServicesIngressesImages(t *testing.T) {
	traefik, other := "traefik", "cilium"
	cs := fake.NewClientset(
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP}},
		&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "shop"}, Spec: networkingv1.IngressSpec{IngressClassName: &traefik}},
		&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "shop"}},
		&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "shop"}, Spec: networkingv1.IngressSpec{IngressClassName: &other}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "local-path-provisioner", Namespace: "local-path-storage"},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: "rancher/local-path-provisioner:v0.0.37"}}}}}},
	)
	c := &Client{cs: cs}
	ctx := context.Background()
	if got, err := c.LoadBalancerServices(ctx); err != nil || len(got) != 1 || got[0] != "shop/web" {
		t.Errorf("LoadBalancer Services %v, %v", got, err)
	}
	if got, _ := c.Ingresses(ctx, "traefik", false); len(got) != 1 || got[0] != "shop/a" {
		t.Errorf("traefik's Ingresses %v", got)
	}
	if got, _ := c.Ingresses(ctx, "traefik", true); len(got) != 2 {
		t.Errorf("the default class also serves those naming none: %v", got)
	}
	if img, err := c.DeploymentImage(ctx, "local-path-storage", "local-path-provisioner"); err != nil || img != "rancher/local-path-provisioner:v0.0.37" {
		t.Errorf("image %q, %v", img, err)
	}
	if img, err := c.DeploymentImage(ctx, "kube-system", "nope"); err != nil || img != "" {
		t.Errorf("a missing Deployment has no image: %q, %v", img, err)
	}
}

// A machine leaving: its Node goes (one already gone is fine), and the local volumes tied
// to it are found.
func TestNodeLeaving(t *testing.T) {
	pinned := func(name, node string) *corev1.PersistentVolume {
		return &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{"pv.kubernetes.io/provisioned-by": "rancher.io/local-path"}},
			Spec: corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{Namespace: "shop", Name: "data-" + name},
				NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
					{Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn, Values: []string{node}}}}}}}}}
	}
	cs := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w-1"}}, pinned("pv-a", "w-1"), pinned("pv-b", "w-0"))
	c := &Client{cs: cs}
	ctx := context.Background()
	if got, err := c.LocalVolumesOn(ctx, "w-1", []string{"rancher.io/local-path"}); err != nil || len(got) != 1 || got[0] != "shop/data-pv-a" {
		t.Errorf("volumes on w-1: %v, %v", got, err)
	}
	if err := c.DeleteNode(ctx, "w-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteNode(ctx, "w-1"); err != nil {
		t.Errorf("a node already gone: %v", err)
	}
}

func fast(t *testing.T) {
	t.Helper()
	every, timeout := pollEvery, waitTimeout
	pollEvery, waitTimeout = time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { pollEvery, waitTimeout = every, timeout })
}

func node(name string, ready, cp bool) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"kubernetes.io/hostname": name}}}
	if cp {
		n.Labels[controlPlaneLabel] = "true"
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}
	return n
}

// Health lists every pod that isn't running as it should; a failed pod a controller replaced,
// or a Job's that succeeded since, isn't a problem.
func TestHealth(t *testing.T) {
	pod := func(name string, phase corev1.PodPhase, owner string, mod func(*corev1.Pod)) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps"}, Spec: corev1.PodSpec{NodeName: "w-0"}, Status: corev1.PodStatus{Phase: phase}}
		if kind, name, ok := strings.Cut(owner, "/"); ok {
			yes := true
			p.OwnerReferences = []metav1.OwnerReference{{Kind: kind, Name: name, Controller: &yes}}
		}
		if mod != nil {
			mod(p)
		}
		return p
	}
	waiting := func(reason string) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}}}
		}
	}
	done := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "migrate", Namespace: "apps"}, Status: batchv1.JobStatus{Succeeded: 1}}
	running := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "backup", Namespace: "apps"}}
	now := metav1.Now()
	cs := fake.NewClientset(node("cp-0", true, true), node("w-0", false, false), done, running,
		pod("web", corev1.PodRunning, "ReplicaSet/web", nil),
		pod("crash", corev1.PodRunning, "ReplicaSet/crash", waiting("CrashLoopBackOff")),
		pod("starting", corev1.PodRunning, "", waiting("ContainerCreating")), // not stuck
		pod("pull", corev1.PodPending, "", waiting("ImagePullBackOff")),
		pod("big", corev1.PodPending, "", func(p *corev1.Pod) {
			p.Spec.NodeName = ""
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Message: "0/2 nodes have enough memory"}}
		}),
		pod("bare", corev1.PodFailed, "", func(p *corev1.Pod) { p.Status.Reason = "Evicted" }),
		pod("replaced", corev1.PodFailed, "ReplicaSet/web", nil),
		pod("migrate-1", corev1.PodFailed, "Job/migrate", nil),
		pod("backup-1", corev1.PodFailed, "Job/backup", nil),
		pod("ok", corev1.PodSucceeded, "Job/backup", nil),
		pod("going", corev1.PodPending, "", func(p *corev1.Pod) { p.DeletionTimestamp = &now; p.Finalizers = []string{"x"} }),
	)
	cs.CoreV1().Nodes().UpdateStatus(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp-0", Labels: map[string]string{controlPlaneLabel: ""}},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}, NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.37.2"}}}, metav1.UpdateOptions{})

	h, err := (&Client{cs: cs}).Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Nodes) != 2 || h.Nodes[0] != (Node{Name: "cp-0", ControlPlane: true, Ready: true, Version: "v1.37.2"}) || h.Nodes[1].Ready {
		t.Errorf("nodes %+v", h.Nodes)
	}
	want := []string{
		"apps/backup-1 on w-0: failed",
		"apps/bare on w-0: failed Evicted",
		"apps/big: can't be scheduled: 0/2 nodes have enough memory",
		"apps/crash on w-0: CrashLoopBackOff",
		"apps/pull on w-0: pending (ImagePullBackOff)",
	}
	if strings.Join(h.Problems, "\n") != strings.Join(want, "\n") {
		t.Errorf("problems:\n%s\nwant:\n%s", strings.Join(h.Problems, "\n"), strings.Join(want, "\n"))
	}
}

func TestWaitNodes(t *testing.T) {
	fast(t)
	c := &Client{cs: fake.NewClientset(node("cp-0", true, true), node("w-0", false, false))}
	ctx := context.Background()

	if err := c.WaitNodes(ctx, []string{"cp-0", "w-0"}, false); err != nil {
		t.Errorf("both registered: %v", err)
	}
	err := c.WaitNodes(ctx, []string{"cp-0", "w-0"}, true)
	if err == nil || !strings.Contains(err.Error(), "not Ready") || !strings.Contains(err.Error(), "w-0") || strings.Contains(err.Error(), "cp-0") {
		t.Errorf("w-0 isn't Ready: %v", err)
	}
	err = c.WaitNodes(ctx, []string{"cp-0", "w-1"}, false)
	if err == nil || !strings.Contains(err.Error(), "never joined the cluster: w-1") {
		t.Errorf("w-1 never registered: %v", err)
	}
}

func TestWaitCountAndLabel(t *testing.T) {
	fast(t)
	c := &Client{cs: fake.NewClientset(node("cp-0", true, true), node("w-0", true, false))}
	ctx := context.Background()

	nodes, err := c.WaitCount(ctx, 2)
	if err != nil || len(nodes) != 2 {
		t.Fatalf("nodes = %+v, %v", nodes, err)
	}
	for _, n := range nodes {
		if n.ControlPlane != (n.Name == "cp-0") {
			t.Errorf("%s: control plane = %v", n.Name, n.ControlPlane)
		}
	}
	if _, err := c.WaitCount(ctx, 3); err == nil || !strings.Contains(err.Error(), "2 of 3 nodes registered") {
		t.Errorf("a third node never comes: %v", err)
	}

	if err := c.Label(ctx, "w-0", map[string]string{"node-role.kubernetes.io/gpu": ""}); err != nil {
		t.Fatal(err)
	}
	got, _ := c.cs.CoreV1().Nodes().Get(ctx, "w-0", metav1.GetOptions{})
	if _, ok := got.Labels["node-role.kubernetes.io/gpu"]; !ok || got.Labels["kubernetes.io/hostname"] != "w-0" {
		t.Errorf("labels after patch: %v", got.Labels)
	}
	if err := c.Label(ctx, "w-0", nil); err != nil {
		t.Errorf("no labels is a no-op: %v", err)
	}
}

// Cloud volumes are found by their driver; deleting them takes the pods using their claims
// first, then the claims (the driver, faked here, deletes the volume with its claim).
func TestVolumes(t *testing.T) {
	fast(t)
	pv := func(name, driver, claim string) *corev1.PersistentVolume {
		v := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource:        corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: driver}},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
		}}
		if claim != "" {
			v.Spec.ClaimRef = &corev1.ObjectReference{Namespace: "shop", Name: claim}
		}
		return v
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "db-0", Namespace: "shop"}, Spec: corev1.PodSpec{Volumes: []corev1.Volume{
		{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-db-0"}}}}}}
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-db-0", Namespace: "shop"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pvc-1"}}
	cs := fake.NewClientset(pv("pvc-1", "ebs.csi.aws.com", "data-db-0"), pv("pvc-2", "ebs.csi.aws.com", ""), pv("pvc-3", "driver.longhorn.io", "x"), pod, claim)
	cs.PrependReactor("delete", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		cs.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("persistentvolumes"), "", "pvc-1")
		return false, nil, nil
	})
	c := &Client{cs: cs}
	ctx := context.Background()

	vols, err := c.VolumesOf(ctx, []string{"ebs.csi.aws.com"})
	if err != nil || len(vols) != 2 || vols[0].Claim != "shop/data-db-0" || vols[0].Size != "1Gi" || vols[1].Claim != "" {
		t.Fatalf("volumes %+v, %v", vols, err)
	}
	if err := c.DeleteVolumes(ctx, vols); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Pods("shop").Get(ctx, "db-0", metav1.GetOptions{}); err == nil {
		t.Error("the pod using the claim must go first")
	}
	if left, _ := c.VolumesOf(ctx, []string{"ebs.csi.aws.com", "driver.longhorn.io"}); len(left) != 1 || left[0].Name != "pvc-3" {
		t.Errorf("left %+v", left)
	}

	// local-path's volumes aren't CSI: their provisioner signs them.
	local := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pvc-local", Annotations: map[string]string{"pv.kubernetes.io/provisioned-by": "rancher.io/local-path"}},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/opt/local-path-provisioner/x"}}}}
	c = &Client{cs: fake.NewClientset(local, pv("pvc-ebs", "ebs.csi.aws.com", ""))}
	if got, err := c.VolumesOf(ctx, []string{"rancher.io/local-path"}); err != nil || len(got) != 1 || got[0].Name != "pvc-local" || got[0].Driver != "rancher.io/local-path" {
		t.Errorf("local volumes %+v, %v", got, err)
	}

	// A volume that doesn't go away is named.
	cs = fake.NewClientset(pv("stuck", "ebs.csi.aws.com", "data"))
	c = &Client{cs: cs}
	cs.PrependReactor("delete", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, nil })
	if err := c.DeleteVolumes(ctx, []Volume{{Name: "stuck", Claim: "shop/data"}}); err == nil || !strings.Contains(err.Error(), "still there") || !strings.Contains(err.Error(), "stuck") {
		t.Errorf("err = %v", err)
	}
}

func TestSpreadOverNodes(t *testing.T) {
	fast(t)
	sel := map[string]string{"k8s-app": "kube-dns"}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "coredns", Namespace: "kube-system"}, Spec: appsv1.DeploymentSpec{
		Selector: &metav1.LabelSelector{MatchLabels: sel},
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "coredns"}}}},
	}}
	c := &Client{cs: fake.NewClientset(d)}
	ctx := context.Background()
	if err := c.SpreadOverNodes(ctx, "kube-system", "coredns"); err != nil {
		t.Fatal(err)
	}
	got, _ := c.cs.AppsV1().Deployments("kube-system").Get(ctx, "coredns", metav1.GetOptions{})
	tsc := got.Spec.Template.Spec.TopologySpreadConstraints
	if len(tsc) != 2 || tsc[0].TopologyKey != "kubernetes.io/hostname" || tsc[0].WhenUnsatisfiable != corev1.DoNotSchedule || tsc[0].MaxSkew != 1 ||
		tsc[0].LabelSelector == nil || tsc[0].LabelSelector.MatchLabels["k8s-app"] != "kube-dns" ||
		tsc[1].TopologyKey != "topology.kubernetes.io/zone" || tsc[1].WhenUnsatisfiable != corev1.ScheduleAnyway {
		t.Errorf("constraints: %+v", tsc)
	}
	if len(got.Spec.Template.Spec.Containers) != 1 {
		t.Errorf("the patch replaced the pod spec: %+v", got.Spec.Template.Spec)
	}
	if err := c.SpreadOverNodes(ctx, "kube-system", "gone"); err == nil || !strings.Contains(err.Error(), "never appeared") {
		t.Errorf("err = %v", err)
	}
}

func TestScaleUp(t *testing.T) {
	fast(t)
	replicas := func(n int32) *int32 { return &n }
	deployment := func(name string, n *int32) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system"}, Spec: appsv1.DeploymentSpec{Replicas: n}}
	}
	c := &Client{cs: fake.NewClientset(deployment("coredns", nil), deployment("big", replicas(3)))}
	ctx := context.Background()
	get := func(name string) int32 {
		d, _ := c.cs.AppsV1().Deployments("kube-system").Get(ctx, name, metav1.GetOptions{})
		return *d.Spec.Replicas
	}

	// No replicas field (k3s's CoreDNS) means one: scaled up.
	if err := c.ScaleUp(ctx, "kube-system", "coredns", 2); err != nil || get("coredns") != 2 {
		t.Errorf("coredns: %d, %v", get("coredns"), err)
	}
	// Never scaled down.
	if err := c.ScaleUp(ctx, "kube-system", "big", 2); err != nil || get("big") != 3 {
		t.Errorf("big: %d, %v", get("big"), err)
	}
	if err := c.ScaleUp(ctx, "kube-system", "gone", 2); err == nil || !strings.Contains(err.Error(), "Deployment kube-system/gone never appeared") {
		t.Errorf("err = %v", err)
	}
}

// Labels and taints change one by one: what isn't named stays as it is.
func TestRelabelAndRetaint(t *testing.T) {
	n := node("w-0", true, false)
	n.Labels["tier"], n.Labels["old"], n.Labels["cloud.example/zone"] = "web", "x", "a"
	n.Spec.Taints = []corev1.Taint{
		{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule},
		{Key: "gone", Effect: corev1.TaintEffectNoExecute},
		{Key: "node.kubernetes.io/unreachable", Effect: corev1.TaintEffectNoExecute},
	}
	n.Status.NodeInfo.KubeletVersion, n.Spec.Unschedulable = "v1.37.2+k3s1", true
	c := &Client{cs: fake.NewClientset(n)}
	ctx := context.Background()

	if err := c.Relabel(ctx, "w-0", map[string]string{"tier": "batch"}, []string{"old"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Retaint(ctx, "w-0", []Taint{{Key: "dedicated", Value: "ml", Effect: "NoSchedule"}, {Key: "new", Effect: "PreferNoSchedule"}}, []string{"gone:NoExecute"}); err != nil {
		t.Fatal(err)
	}
	metas, err := c.NodeMetas(ctx)
	if err != nil || len(metas) != 1 {
		t.Fatalf("%v, %v", metas, err)
	}
	m := metas[0]
	if m.Version != "v1.37.2+k3s1" || !m.Ready || !m.Unschedulable {
		t.Errorf("release %q, ready %v, cordoned %v", m.Version, m.Ready, m.Unschedulable)
	}
	if m.Labels["tier"] != "batch" || m.Labels["cloud.example/zone"] != "a" || m.Labels["kubernetes.io/hostname"] != "w-0" {
		t.Errorf("labels = %v", m.Labels)
	}
	if _, ok := m.Labels["old"]; ok {
		t.Error("label old should be gone")
	}
	var got []string
	for _, t := range m.Taints {
		got = append(got, t.String())
	}
	if want := "node.kubernetes.io/unreachable:NoExecute dedicated=ml:NoSchedule new:PreferNoSchedule"; strings.Join(got, " ") != want {
		t.Errorf("taints = %v, want %s", got, want)
	}
}

// owned is a pod of a controller, on a node, Ready or not.
func owned(name, node, kind, owner string, isReady bool) *corev1.Pod {
	yes := true
	status := corev1.ConditionFalse
	if isReady {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "db", OwnerReferences: []metav1.OwnerReference{
			{Kind: kind, Name: owner, UID: types.UID("uid-" + owner), Controller: &yes}}},
		Spec:   corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}},
	}
}

// Draining a node waits for the workloads with a pod there whose other pods aren't Ready:
// the lab's case, a database primary on w-1 while its replica on w-0 is still catching up.
// What isn't the node's own business doesn't hold it back.
func TestUnsettled(t *testing.T) {
	pollEvery = time.Millisecond
	primary, replica := owned("db-1", "w-1", "Cluster", "db", true), owned("db-2", "w-0", "Cluster", "db", false)
	cs := fake.NewClientset(primary, replica,
		owned("web-a", "w-1", "ReplicaSet", "web", true), owned("web-b", "w-0", "ReplicaSet", "web", true), // all Ready
		owned("other-a", "w-0", "ReplicaSet", "other", false),                                                     // not Ready, but nothing of it on w-1
		owned("agent-a", "w-1", "DaemonSet", "agent", true), owned("agent-b", "w-0", "DaemonSet", "agent", false), // drains leave DaemonSets
		owned("pending", "", "ReplicaSet", "web", false)) // web's new pod, not scheduled yet
	c := &Client{cs: cs}

	got, err := c.Unsettled(context.Background(), "w-1")
	want := []string{"db/Cluster db: its pod db-2 (on w-0) isn't Ready", "db/ReplicaSet web: its pod pending (not scheduled yet) isn't Ready"}
	if err != nil || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("Unsettled(w-1) = %q, %v; want %q", got, err, want)
	}
	// On w-0: the replica's own state and "other" (nothing of it elsewhere) don't hold the drain;
	// web, with a pod there and one not scheduled yet, does.
	if got, _ := c.Unsettled(context.Background(), "w-0"); strings.Join(got, "\n") != want[1] {
		t.Errorf("Unsettled(w-0) = %q, want only %q", got, want[1])
	}

	waiting, err := c.WaitSettled(context.Background(), "w-1", 20*time.Millisecond)
	if err == nil || len(waiting) != 2 {
		t.Errorf("WaitSettled gives up after its timeout, naming what it waited for: %q, %v", waiting, err)
	}
	// The replica catches up, the new web pod runs: the drain may go.
	replica.Status.Conditions[0].Status = corev1.ConditionTrue
	cs.CoreV1().Pods("db").Update(context.Background(), replica, metav1.UpdateOptions{})
	cs.CoreV1().Pods("db").Delete(context.Background(), "pending", metav1.DeleteOptions{})
	if waiting, err := c.WaitSettled(context.Background(), "w-1", time.Second); err != nil || len(waiting) != 0 {
		t.Errorf("settled: %q, %v", waiting, err)
	}
}
