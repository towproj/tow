package integration

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/towproj/tow/internal/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// usesChart reports whether the file installs the chart (by its chart name).
func usesChart(cfg *config.Config, chart string) bool {
	return slices.ContainsFunc(cfg.Spec.Kubernetes.Addons.HelmCharts, func(h config.HelmChart) bool { return h.Chart == chart })
}

// checkPostgres proves a database survives losing its primary's machine (on AWS, its zone):
// CloudNativePG's Cluster "db" (examples/manifests/postgres.yaml) runs three instances on three
// machines; a row written through the read-write Service db-rw is still read there once the
// primary's machine is powered off and a replica took over. The machine then comes back, and
// the cluster has its three instances again.
func checkPostgres(t *testing.T, ctx context.Context, c clusterCheck, off, on func(ctx context.Context, node string) error) {
	t.Helper()
	cs := clientset(t, c.st)
	const ns = "postgres"
	instances := func() (primary corev1.Pod, ready []corev1.Pod) {
		list, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "cnpg.io/cluster=db,cnpg.io/podRole=instance"})
		if err != nil {
			return
		}
		for _, p := range list.Items {
			if p.DeletionTimestamp != nil || !podReady(p) {
				continue
			}
			ready = append(ready, p)
			if p.Labels["cnpg.io/instanceRole"] == "primary" {
				primary = p
			}
		}
		return
	}
	spread := func() bool {
		primary, ready := instances()
		machines := map[string]bool{}
		for _, p := range ready {
			machines[p.Spec.NodeName] = true
		}
		return primary.Name != "" && len(machines) == 3
	}
	eventuallyWithin(t, ctx, 15*time.Minute, "3 Postgres instances Ready, one per machine, one of them primary", spread)
	primary, _ := instances()
	image, victim := primary.Spec.Containers[0].Image, primary.Spec.NodeName

	// The row goes in through db-rw, from a pod of the instances' own image (it has psql).
	const row = "written before the failover"
	n := 0
	query := func(sql string) (string, error) {
		n++
		return runPsql(ctx, cs, ns, fmt.Sprintf("psql-%d", n), image, victim, sql)
	}
	var out string
	var err error
	eventuallyWithin(t, ctx, 3*time.Minute, "a row written through db-rw", func() bool {
		out, err = query("CREATE TABLE IF NOT EXISTS checks (note text); INSERT INTO checks VALUES ('" + row + "')")
		return err == nil
	})

	t.Logf("powering off %s, which runs the primary %s", victim, primary.Name)
	start := time.Now()
	if err := off(ctx, victim); err != nil {
		t.Fatalf("power off %s: %v", victim, err)
	}
	eventuallyWithin(t, ctx, 5*time.Minute, "a replica on another machine promoted", func() bool {
		p, _ := instances()
		return p.Name != "" && p.Spec.NodeName != victim
	})
	eventuallyWithin(t, ctx, 5*time.Minute, "the row read through db-rw after the failover", func() bool {
		out, err = query("SELECT note FROM checks")
		return err == nil && strings.Contains(out, row)
	})
	now, _ := instances()
	t.Logf("%s took over in %s; the row is there", now.Name, time.Since(start).Round(time.Second))

	if err := on(ctx, victim); err != nil {
		t.Fatalf("power on %s: %v", victim, err)
	}
	eventuallyWithin(t, ctx, 10*time.Minute, "3 Postgres instances Ready again, one per machine", spread)
}

// runPsql runs one psql command against db-rw, with the app's credentials from the Secret
// CloudNativePG made, in a pod kept off the machine avoid (it may be powered off), and returns
// what psql printed.
func runPsql(ctx context.Context, cs kubernetes.Interface, ns, name, image, avoid, sql string) (string, error) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{
					{Key: "metadata.name", Operator: corev1.NodeSelectorOpNotIn, Values: []string{avoid}}}}}}}},
			Containers: []corev1.Container{{Name: "psql", Image: image,
				Command: []string{"psql", "$(URI)", "-tA", "-c", sql},
				Env: []corev1.EnvVar{{Name: "URI", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "db-app"}, Key: "uri"}}}}}},
		},
	}
	if _, err := cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return "", err
	}
	defer cs.CoreV1().Pods(ns).Delete(context.WithoutCancel(ctx), name, metav1.DeleteOptions{})
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	for {
		p, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil && (p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed) {
			logs, _ := cs.CoreV1().Pods(ns).GetLogs(name, &corev1.PodLogOptions{}).DoRaw(ctx)
			if p.Status.Phase == corev1.PodFailed {
				return string(logs), fmt.Errorf("psql failed: %s", logs)
			}
			return string(logs), nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("psql pod %s didn't finish", name)
		case <-time.After(3 * time.Second):
		}
	}
}

// podReady reports whether a pod's Ready condition is true.
func podReady(p corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
