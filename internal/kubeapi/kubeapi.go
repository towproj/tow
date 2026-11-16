// Package kubeapi talks to a cluster's API from this machine through the kubeconfig tow
// saved, the way kubectl would: when it works, the kubeconfig the user got works too.
package kubeapi

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
)

// Vars so tests don't wait.
var (
	pollEvery   = 5 * time.Second
	waitTimeout = 10 * time.Minute // per wait: a new node pulls its CNI's images before it's Ready
)

// controlPlaneLabel is the label kubeadm and k3s put on control-plane nodes.
const controlPlaneLabel = "node-role.kubernetes.io/control-plane"

type Client struct {
	cs   kubernetes.Interface
	host string
}

// Node is what tow needs to know about a registered node.
type Node struct {
	Name         string
	ControlPlane bool
	Ready        bool
	Version      string // the kubelet's
}

// New builds a client from a kubeconfig's current context.
func New(kubeconfig []byte) (*Client, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig: %w", err)
	}
	cfg.Timeout = 15 * time.Second // per request: a silent API must not hang a wait
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{cs: cs, host: cfg.Host}, nil
}

// WaitReachable waits until the API answers. A new load balancer's name can take a minute
// to resolve, and its health checks a minute more to send traffic.
func (c *Client) WaitReachable(ctx context.Context) error {
	var last error
	err := poll(ctx, func() bool {
		_, last = c.cs.Discovery().ServerVersion()
		return last == nil
	})
	if err != nil {
		return fmt.Errorf("the Kubernetes API at %s doesn't answer from this machine: %v\n  → is port 6443 open to this machine (firewall, security group)?", c.host, last)
	}
	return nil
}

// WaitNodes waits until every named node is registered, and Ready too when ready is set.
func (c *Client) WaitNodes(ctx context.Context, names []string, ready bool) error {
	var missing []string
	var last error
	err := poll(ctx, func() bool {
		var nodes []Node
		nodes, last = c.nodes(ctx)
		if last != nil {
			return false
		}
		missing = missing[:0]
		for _, name := range names {
			i := slices.IndexFunc(nodes, func(n Node) bool { return n.Name == name })
			if i < 0 || (ready && !nodes[i].Ready) {
				missing = append(missing, name)
			}
		}
		return len(missing) == 0
	})
	switch {
	case err == nil:
		return nil
	case last != nil:
		return fmt.Errorf("could not list the nodes: %w", last)
	case ready:
		return fmt.Errorf("not Ready after %s: %s", waitTimeout, strings.Join(missing, ", "))
	default:
		return fmt.Errorf("never joined the cluster: %s", strings.Join(missing, ", "))
	}
}

// WaitCount waits until n nodes are registered and returns them (k3d names its own nodes).
func (c *Client) WaitCount(ctx context.Context, n int) ([]Node, error) {
	var nodes []Node
	var last error
	err := poll(ctx, func() bool {
		nodes, last = c.nodes(ctx)
		return last == nil && len(nodes) >= n
	})
	if err != nil {
		if last != nil {
			return nil, fmt.Errorf("could not list the nodes: %w", last)
		}
		return nil, fmt.Errorf("%d of %d nodes registered after %s", len(nodes), n, waitTimeout)
	}
	return nodes, nil
}

// Label adds labels to a node, keeping the ones it has.
func (c *Client) Label(ctx context.Context, node string, labels map[string]string) error {
	if len(labels) == 0 {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels}})
	if err != nil {
		return err
	}
	if _, err := c.cs.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("label node %s: %w", node, err)
	}
	return nil
}

// Taint is one node taint; a node has at most one per key and effect.
type Taint struct{ Key, Value, Effect string }

// ID is what makes a taint unique on a node: its key and effect.
func (t Taint) ID() string { return t.Key + ":" + t.Effect }

func (t Taint) String() string {
	if t.Value == "" {
		return t.ID()
	}
	return t.Key + "=" + t.Value + ":" + t.Effect
}

// NodeMeta is a registered node's labels and taints, its kubelet's release and its state.
type NodeMeta struct {
	Name          string
	Labels        map[string]string
	Taints        []Taint
	Version       string // the kubelet's: v1.37.2, v1.37.2+k3s1
	Ready         bool
	Unschedulable bool // cordoned
}

// NodeMetas reads every node's labels and taints, release and state.
func (c *Client) NodeMetas(ctx context.Context) ([]NodeMeta, error) {
	list, err := c.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]NodeMeta, 0, len(list.Items))
	for _, n := range list.Items {
		m := NodeMeta{Name: n.Name, Labels: n.Labels, Version: n.Status.NodeInfo.KubeletVersion, Ready: ready(n), Unschedulable: n.Spec.Unschedulable}
		for _, t := range n.Spec.Taints {
			m.Taints = append(m.Taints, Taint{Key: t.Key, Value: t.Value, Effect: string(t.Effect)})
		}
		out = append(out, m)
	}
	return out, nil
}

// Relabel sets labels on a node and removes others, leaving the rest as they are.
func (c *Client) Relabel(ctx context.Context, node string, set map[string]string, remove []string) error {
	labels := map[string]any{}
	for k, v := range set {
		labels[k] = v
	}
	for _, k := range remove {
		labels[k] = nil // a merge patch removes a key set to null
	}
	if len(labels) == 0 {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels}})
	if err != nil {
		return err
	}
	if _, err := c.cs.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("label node %s: %w", node, err)
	}
	return nil
}

// Retaint adds taints to a node (replacing one with the same key and effect) and removes
// others by key and effect, leaving the rest as they are. Taints are a list, so the node is
// read and written back, again if someone else changed it in between.
func (c *Client) Retaint(ctx context.Context, node string, add []Taint, remove []string) error {
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		n, err := c.cs.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
		if err != nil {
			return err
		}
		drop := map[string]bool{}
		for _, id := range remove {
			drop[id] = true
		}
		for _, t := range add {
			drop[t.ID()] = true
		}
		var taints []corev1.Taint
		for _, t := range n.Spec.Taints {
			if !drop[t.Key+":"+string(t.Effect)] {
				taints = append(taints, t)
			}
		}
		for _, t := range add {
			taints = append(taints, corev1.Taint{Key: t.Key, Value: t.Value, Effect: corev1.TaintEffect(t.Effect)})
		}
		n.Spec.Taints = taints
		_, err = c.cs.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("taint node %s: %w", node, err)
	}
	return nil
}

// ScaleUp waits for a Deployment to exist, then gives it at least replicas replicas; one that
// already has as many is left alone.
func (c *Client) ScaleUp(ctx context.Context, namespace, name string, replicas int32) error {
	var have int32
	var last error
	if err := poll(ctx, func() bool {
		d, err := c.cs.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if last = err; err != nil {
			return false
		}
		have = 1
		if d.Spec.Replicas != nil {
			have = *d.Spec.Replicas
		}
		return true
	}); err != nil {
		return fmt.Errorf("Deployment %s/%s never appeared: %v", namespace, name, last)
	}
	if have >= replicas {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"replicas": replicas}})
	if err != nil {
		return err
	}
	if _, err := c.cs.AppsV1().Deployments(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("scale Deployment %s/%s to %d replicas: %w", namespace, name, replicas, err)
	}
	return nil
}

// SpreadOverNodes gives a Deployment the spread rule k3s's own CoreDNS has: never two of its
// pods on one node while another node has none (they wait for that node instead), and spread
// over zones when it can. kubeadm's CoreDNS only prefers different nodes, so its two replicas
// can share one: losing that node would leave the cluster without DNS.
func (c *Client) SpreadOverNodes(ctx context.Context, namespace, name string) error {
	var d *appsv1.Deployment
	var last error
	if err := poll(ctx, func() bool {
		d, last = c.cs.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		return last == nil
	}); err != nil {
		return fmt.Errorf("Deployment %s/%s never appeared: %v", namespace, name, last)
	}
	selector := d.Spec.Selector.MatchLabels
	rule := func(key, when string) map[string]any {
		return map[string]any{"maxSkew": 1, "topologyKey": key, "whenUnsatisfiable": when, "labelSelector": map[string]any{"matchLabels": selector}}
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
		"topologySpreadConstraints": []any{rule("kubernetes.io/hostname", "DoNotSchedule"), rule("topology.kubernetes.io/zone", "ScheduleAnyway")},
	}}}})
	if err != nil {
		return err
	}
	if _, err := c.cs.AppsV1().Deployments(namespace).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("spread Deployment %s/%s over the nodes: %w", namespace, name, err)
	}
	return nil
}

// Volume is a PersistentVolume of a storage driver, with the claim bound to it.
type Volume struct {
	Name, Claim string // the PersistentVolume, and namespace/claim ("" when none)
	Driver      string
	Size        string
	Reclaim     string // Delete: the driver deletes the disk with the claim
}

// VolumesOf lists the PersistentVolumes the drivers made.
func (c *Client) VolumesOf(ctx context.Context, drivers []string) ([]Volume, error) {
	list, err := c.cs.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []Volume
	for _, pv := range list.Items {
		// A CSI volume names its driver; local-path's and OpenEBS's hostpath volumes aren't
		// CSI: their provisioner signs them.
		driver := pv.Annotations["pv.kubernetes.io/provisioned-by"]
		if pv.Spec.CSI != nil {
			driver = pv.Spec.CSI.Driver
		}
		if !slices.Contains(drivers, driver) {
			continue
		}
		v := Volume{Name: pv.Name, Driver: driver, Reclaim: string(pv.Spec.PersistentVolumeReclaimPolicy)}
		if q, ok := pv.Spec.Capacity[corev1.ResourceStorage]; ok {
			v.Size = q.String()
		}
		if r := pv.Spec.ClaimRef; r != nil {
			v.Claim = r.Namespace + "/" + r.Name
		}
		out = append(out, v)
	}
	return out, nil
}

// DeleteVolumes deletes the volumes through Kubernetes, so their drivers delete the disks: the
// pods using their claims first (a claim in use isn't deleted), then the claims, then
// the volumes left without one; it waits until every volume is gone.
func (c *Client) DeleteVolumes(ctx context.Context, vols []Volume) error {
	claims := map[string]bool{}
	for _, v := range vols {
		if v.Claim != "" {
			claims[v.Claim] = true
		}
	}
	pods, err := c.cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, p := range pods.Items {
		for _, vol := range p.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil && claims[p.Namespace+"/"+vol.PersistentVolumeClaim.ClaimName] {
				if err := c.cs.CoreV1().Pods(p.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
					return fmt.Errorf("delete pod %s/%s: %w", p.Namespace, p.Name, err)
				}
				break
			}
		}
	}
	for _, v := range vols {
		var err error
		if ns, name, ok := strings.Cut(v.Claim, "/"); ok {
			err = c.cs.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, name, metav1.DeleteOptions{})
		} else {
			err = c.cs.CoreV1().PersistentVolumes().Delete(ctx, v.Name, metav1.DeleteOptions{})
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete volume %s: %w", v.Name, err)
		}
	}
	var left []string
	if err := poll(ctx, func() bool {
		left = left[:0]
		for _, v := range vols {
			if _, err := c.cs.CoreV1().PersistentVolumes().Get(ctx, v.Name, metav1.GetOptions{}); err == nil || !apierrors.IsNotFound(err) {
				left = append(left, v.Name)
			}
		}
		return len(left) == 0
	}); err != nil {
		return fmt.Errorf("these volumes are still there after %s: %s", waitTimeout, strings.Join(left, ", "))
	}
	return nil
}

// LoadBalancerServices lists the Services of type LoadBalancer, as namespace/name.
func (c *Client) LoadBalancerServices(ctx context.Context) ([]string, error) {
	list, err := c.cs.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range list.Items {
		if s.Spec.Type == corev1.ServiceTypeLoadBalancer {
			out = append(out, s.Namespace+"/"+s.Name)
		}
	}
	return out, nil
}

// Ingresses lists the Ingresses a class serves, as namespace/name: those that name it, and
// those that name none when it's the default.
func (c *Client) Ingresses(ctx context.Context, class string, isDefault bool) ([]string, error) {
	list, err := c.cs.NetworkingV1().Ingresses("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, in := range list.Items {
		name := ""
		if in.Spec.IngressClassName != nil {
			name = *in.Spec.IngressClassName
		}
		if name == class || (name == "" && isDefault) {
			out = append(out, in.Namespace+"/"+in.Name)
		}
	}
	return out, nil
}

// DeleteNode deletes a Node object; one already gone is fine.
func (c *Client) DeleteNode(ctx context.Context, name string) error {
	err := c.cs.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// LocalVolumesOn lists the volumes of these drivers whose data lives on a node's own disk (a
// PersistentVolume tied to that node), as namespace/claim, or the volume's name.
func (c *Client) LocalVolumesOn(ctx context.Context, node string, drivers []string) ([]string, error) {
	vols, err := c.VolumesOf(ctx, drivers)
	if err != nil {
		return nil, err
	}
	list, err := c.cs.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	on := map[string]bool{}
	for _, pv := range list.Items {
		if a := pv.Spec.NodeAffinity; a != nil && a.Required != nil {
			for _, term := range a.Required.NodeSelectorTerms {
				for _, e := range term.MatchExpressions {
					if e.Key == "kubernetes.io/hostname" && e.Operator == corev1.NodeSelectorOpIn && slices.Contains(e.Values, node) {
						on[pv.Name] = true
					}
				}
			}
		}
	}
	var out []string
	for _, v := range vols {
		if on[v.Name] {
			out = append(out, cmp.Or(v.Claim, v.Name))
		}
	}
	return out, nil
}

// DeploymentImage is the image of a Deployment's first container ("" when the Deployment
// doesn't exist).
func (c *Client) DeploymentImage(ctx context.Context, namespace, name string) (string, error) {
	d, err := c.cs.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return "", nil
	case err != nil:
		return "", err
	case len(d.Spec.Template.Spec.Containers) == 0:
		return "", nil
	}
	return d.Spec.Template.Spec.Containers[0].Image, nil
}

func (c *Client) nodes(ctx context.Context) ([]Node, error) {
	list, err := c.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(list.Items))
	for _, n := range list.Items {
		_, cp := n.Labels[controlPlaneLabel]
		out = append(out, Node{Name: n.Name, ControlPlane: cp, Ready: ready(n), Version: n.Status.NodeInfo.KubeletVersion})
	}
	return out, nil
}

// Health is the cluster as 'tow status' shows it: its nodes, and every pod that isn't
// running as it should, in any namespace.
type Health struct {
	Nodes    []Node
	Problems []string // "namespace/pod on node: why"
}

// stuck are the reasons a container waits that won't go away by themselves.
var stuck = []string{"CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError", "CreateContainerError", "RunContainerError", "InvalidImageName"}

// Health reads the nodes and the pods. A failed pod a controller replaced (a Deployment's,
// a DaemonSet's) isn't a problem, nor one of a Job that succeeded since: their replacement
// says how things are.
func (c *Client) Health(ctx context.Context) (Health, error) {
	nodes, err := c.nodes(ctx)
	if err != nil {
		return Health{}, err
	}
	pods, err := c.cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return Health{}, err
	}
	jobs, err := c.cs.BatchV1().Jobs("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return Health{}, err
	}
	succeeded := map[string]bool{}
	for _, j := range jobs.Items {
		succeeded[j.Namespace+"/"+j.Name] = j.Status.Succeeded > 0
	}
	h := Health{Nodes: nodes}
	for _, p := range pods.Items {
		if p.DeletionTimestamp != nil {
			continue
		}
		why := podProblem(p)
		if p.Status.Phase == corev1.PodFailed {
			if owner := metav1.GetControllerOf(&p); owner != nil && (owner.Kind != "Job" || succeeded[p.Namespace+"/"+owner.Name]) {
				why = ""
			}
		}
		if why != "" {
			where := p.Namespace + "/" + p.Name
			if p.Spec.NodeName != "" {
				where += " on " + p.Spec.NodeName
			}
			h.Problems = append(h.Problems, where+": "+why)
		}
	}
	slices.Sort(h.Problems)
	return h, nil
}

// Unsettled names the workloads that have a pod on node and another one, elsewhere, that
// isn't Ready: draining node now would take a second pod of theirs away, before the first
// one came back. A workload is its pods' controller (a ReplicaSet, a StatefulSet, an
// operator's object such as CloudNativePG's Cluster). A DaemonSet's pods (drains leave
// them), mirror pods and finished pods don't count. "Ready" is the pod's own readiness:
// what its readiness probe says (a database's can mean "caught up").
func (c *Client) Unsettled(ctx context.Context, node string) ([]string, error) {
	pods, err := c.cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	controller := func(p corev1.Pod) *metav1.OwnerReference {
		ref := metav1.GetControllerOf(&p)
		if ref == nil || ref.Kind == "DaemonSet" || p.Annotations[corev1.MirrorPodAnnotationKey] != "" ||
			p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			return nil
		}
		return ref
	}
	onNode := map[types.UID]bool{}
	for _, p := range pods.Items {
		if ref := controller(p); ref != nil && p.Spec.NodeName == node {
			onNode[ref.UID] = true
		}
	}
	var out []string
	for _, p := range pods.Items {
		ref := controller(p)
		if ref == nil || !onNode[ref.UID] || p.Spec.NodeName == node || podReady(p) {
			continue
		}
		where := "not scheduled yet"
		if p.Spec.NodeName != "" {
			where = "on " + p.Spec.NodeName
		}
		out = append(out, fmt.Sprintf("%s/%s %s: its pod %s (%s) isn't Ready", p.Namespace, ref.Kind, ref.Name, p.Name, where))
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// WaitSettled waits, for at most timeout, until no workload with a pod on node has another
// one that isn't Ready (Unsettled); it returns those still waited for when time runs out.
func (c *Client) WaitSettled(ctx context.Context, node string, timeout time.Duration) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var waiting []string
	for {
		var err error
		if waiting, err = c.Unsettled(ctx, node); err == nil && len(waiting) == 0 {
			return nil, nil
		}
		select {
		case <-ctx.Done():
			if err != nil && len(waiting) == 0 {
				return nil, err
			}
			return waiting, ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}

func podReady(p corev1.Pod) bool {
	if p.DeletionTimestamp != nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// podProblem says what's wrong with a pod, "" when nothing is.
func podProblem(p corev1.Pod) string {
	waiting := func() string {
		for _, s := range append(slices.Clone(p.Status.InitContainerStatuses), p.Status.ContainerStatuses...) {
			if w := s.State.Waiting; w != nil && w.Reason != "" {
				return w.Reason
			}
		}
		return ""
	}
	switch p.Status.Phase {
	case corev1.PodSucceeded:
		return ""
	case corev1.PodFailed:
		return strings.TrimSpace("failed " + p.Status.Reason)
	case corev1.PodUnknown:
		return "state unknown: its node doesn't answer"
	case corev1.PodPending:
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
				return "can't be scheduled: " + c.Message
			}
		}
		if w := waiting(); w != "" {
			return "pending (" + w + ")"
		}
		return "pending"
	}
	if w := waiting(); slices.Contains(stuck, w) {
		return w
	}
	return ""
}

func ready(n corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// poll runs check now and every pollEvery until it holds, for at most waitTimeout.
func poll(ctx context.Context, check func() bool) error {
	ctx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	for !check() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollEvery):
		}
	}
	return nil
}
