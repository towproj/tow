package core

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/k3s"
	"github.com/towproj/tow/internal/kubeadm"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/state"
)

// useSettingsRoll fakes k3s's rolling install: every machine changes inside Rolling.
func useSettingsRoll(t *testing.T) (*upgradeWorld, *[]k3s.Options) {
	t.Helper()
	w := useUpgrade(t)
	var got []k3s.Options
	installK3s = func(ctx context.Context, o k3s.Options) ([]byte, error) {
		got = append(got, o)
		for _, n := range o.Nodes {
			if o.Rolling == nil {
				continue
			}
			if err := o.Rolling(ctx, n, func() error {
				w.calls = append(w.calls, "restart "+n.Name)
				return nil
			}); err != nil {
				return nil, err
			}
		}
		return []byte(k3sKubeconfig), nil
	}
	return w, &got
}

// k3s's new settings: the plan says what changes where; each machine restarts k3s in turn,
// a control plane checked before the next; nothing is drained.
func TestSettingsK3s(t *testing.T) {
	useFakes(t)
	w, installs := useSettingsRoll(t)
	k3sLab := strings.Replace(fmt.Sprintf(lab, "", ""), "distribution: k3s", "distribution: k3s, apiServer: {extraArgs: {audit-log-maxage: 30}}", 1)
	running(t, parse(t, "lab", k3sLab))
	changed := strings.Replace(k3sLab, "audit-log-maxage: 30", "audit-log-maxage: 60", 1)
	changed = strings.Replace(changed, "workers: [{name: general, machines: [{host: 10.0.0.2}]", "workers: [{name: general, machines: [{host: 10.0.0.2}], kubelet: {extraArgs: {max-pods: 200}}", 1)
	cfg := parse(t, "lab", changed)

	var plan updatePlan
	if err := NewOrchestrator(true).planSettings(context.Background(), &plan, cfg, parse(t, "lab", k3sLab), loadState(t, "lab"), nil); err != nil {
		t.Fatal(err)
	}
	lines := strings.Join(plan.cluster[0].lines, "\n")
	for _, want := range []string{
		"no drain, its pods keep running",
		"control planes cp: + kube-apiserver-arg=audit-log-maxage=60, − kube-apiserver-arg=audit-log-maxage=30",
		"workers of pool general (w1): + kubelet-arg=max-pods=200",
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("plan lacks %q:\n%s", want, lines)
		}
	}

	if err := applyYes(t, cfg); err != nil {
		t.Fatal(err)
	}
	want := []string{"restart cp", "ready cp v1.37.2+k3s1", "readyz cp", "restart w1", "ready w1 v1.37.2+k3s1"}
	if !slices.Equal(w.calls, want) {
		t.Errorf("calls %v, want %v", w.calls, want)
	}
	if o := (*installs)[0]; o.Version != "v1.37.2" || !slices.Contains(rendered(o.ServerFlags), "kube-apiserver-arg=audit-log-maxage=60") {
		t.Errorf("install: %s %v", o.Version, rendered(o.ServerFlags))
	}
	if strings.Contains(strings.Join(w.calls, " "), "drain") {
		t.Error("settings never drain")
	}
}

// What a running cluster can't take: k3s's critical values and data-dir, kubeadm's
// settings (for now). Turning k3s's local-path off while it has volumes is refused.
func TestSettingsRefused(t *testing.T) {
	k3sLab := fmt.Sprintf(lab, "", "")
	with := func(extra string) *config.Config {
		return parse(t, "lab", strings.Replace(k3sLab, "distribution: k3s", "distribution: k3s"+extra, 1))
	}
	old := with("")
	for extra, want := range map[string]string{
		", k3s: {server: {extraArgs: {secrets-encryption: true}}}":  "k3s's secrets-encryption must be the same on every server",
		", k3s: {server: {extraArgs: {data-dir: /data}}}":           "k3s's data-dir must be the same",
		", k3s: {agent: {extraArgs: {data-dir: /data}}}":            "k3s's data-dir must be the same",
		", k3s: {server: {extraArgs: {egress-selector-mode: pod}}}": "k3s's egress-selector-mode",
	} {
		got := refusedSettings(old, with(extra), nil)
		if len(got) != 1 || !strings.Contains(got[0], want) {
			t.Errorf("%s: %v", extra, got)
		}
	}
	if got := refusedSettings(old, with(", k3s: {server: {extraArgs: {etcd-snapshot-compress: true}}}"), nil); len(got) != 0 {
		t.Errorf("a plain flag rolls: %v", got)
	}

	kubeadmLab := strings.Replace(k3sLab, "distribution: k3s", "distribution: kubeadm", 1)
	diff := []string{"spec.kubernetes.apiServer.extraArgs.audit-log-maxage: (none) → 30"}
	if got := refusedSettings(parse(t, "lab", kubeadmLab), parse(t, "lab", kubeadmLab), diff); len(got) != 0 {
		t.Errorf("kubeadm's settings roll: %v", got)
	}

	// local-path off with volumes: refused before anything; Traefik off: its Ingresses named.
	useFakes(t)
	useSettingsRoll(t)
	homelab := `{infrastructure: {provider: byoh, access: {ssh: {user: pi}}}, flavor: homelab%s,
  controlPlane: {machines: [{host: 10.0.0.1}]}, workers: [{name: general, machines: [{host: 10.0.0.2}]}]}`
	running(t, parse(t, "lab", fmt.Sprintf(homelab, "")))
	f := useBuiltins(t, &fakeBuiltins{volumes: []string{"shop/data-db-0"}, ingresses: []string{"shop/web"}})
	err := applyYes(t, parse(t, "lab", fmt.Sprintf(homelab, ", kubernetes: {storage: {csi: []}}")))
	if err == nil || !strings.Contains(err.Error(), "local-path still has 1 volume (shop/data-db-0)") {
		t.Errorf("local-path: %v", err)
	}
	f.volumes = nil
	var plan updatePlan
	o := NewOrchestrator(true)
	noTraefik := parse(t, "lab", fmt.Sprintf(homelab, ", kubernetes: {ingress: {controllers: []}}"))
	if err := o.planSettings(context.Background(), &plan, noTraefik, parse(t, "lab", fmt.Sprintf(homelab, "")), loadState(t, "lab"), nil); err != nil {
		t.Fatal(err)
	}
	if len(plan.notes) != 1 || !strings.Contains(plan.notes[0], "the Ingresses Traefik serves lose their controller (1): shop/web") ||
		!strings.Contains(strings.Join(plan.cluster[0].lines, "\n"), "+ disable=traefik") {
		t.Errorf("traefik off: %v %v", plan.notes, plan.cluster)
	}
}

// kubeadm's new settings: the first control plane uploads the configuration and writes its
// changed static pods (a new certificate for new certSANs), the kubelets get their flags and
// configuration (metrics-server on: serving certificates), node by node, each checked; the
// add-ons follow (metrics-server and its CSR approver installed).
func TestSettingsKubeadm(t *testing.T) {
	useFakes(t)
	w, _ := useSettingsRoll(t)
	f := useBuiltins(t, nil)
	var calls []string
	origCP, origKubelet := reconfigureControlPlane, kubeletFlags
	reconfigureControlPlane = func(_ context.Context, _ kubeadm.Options, n providers.Node, r kubeadm.Reconfigure) error {
		calls = append(calls, fmt.Sprintf("cp %s %v sans=%v upload=%v kubelet=%v", n.Name, r.Components, r.SANs, r.Upload, r.Kubelet))
		return nil
	}
	kubeletFlags = func(_ context.Context, _ kubeadm.Options, n providers.Node, removed, added []string, config bool) error {
		calls = append(calls, fmt.Sprintf("kubelet %s -%v +%v config=%v", n.Name, removed, added, config))
		return nil
	}
	t.Cleanup(func() { reconfigureControlPlane, kubeletFlags = origCP, origKubelet })
	w.versions = map[string]string{"cp": "v1.37.2", "w1": "v1.37.2"}

	base := strings.Replace(fmt.Sprintf(lab, "", ""), "distribution: k3s", "distribution: kubeadm, apiServer: {extraArgs: {audit-log-maxage: 30}}", 1)
	running(t, parse(t, "lab", base))
	changed := strings.Replace(base, "audit-log-maxage: 30}}", "audit-log-maxage: 60}, certSANs: [lab.example]}, metricsServer: {enabled: true}", 1)
	changed = strings.Replace(changed, "workers: [{name: general, machines: [{host: 10.0.0.2}]", "workers: [{name: general, machines: [{host: 10.0.0.2}], kubelet: {extraArgs: {max-pods: 200}}", 1)
	if err := applyYes(t, parse(t, "lab", changed)); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"cp cp [apiserver] sans=true upload=true kubelet=true",
		"kubelet cp -[] +[] config=true",
		"kubelet w1 -[] +[--max-pods=200] config=true",
	}
	if !slices.Equal(calls, want) {
		t.Errorf("calls:\n  %s\nwant:\n  %s", strings.Join(calls, "\n  "), strings.Join(want, "\n  "))
	}
	if wantW := []string{"ready cp v1.37.2", "readyz cp", "ready w1 v1.37.2"}; !slices.Equal(w.calls, wantW) {
		t.Errorf("checks %v, want %v", w.calls, wantW)
	}
	if !slices.Contains(f.calls, "install metrics-server") || !slices.Contains(f.calls, "install kubelet-csr-approver") {
		t.Errorf("add-ons: %v", f.calls)
	}
	if got := refusedSettings(parse(t, "lab", base), parse(t, "lab", changed), nil); len(got) != 0 {
		t.Errorf("kubeadm's settings roll now: %v", got)
	}
}

func loadState(t *testing.T, name string) *state.ClusterState {
	t.Helper()
	st, err := state.Load(name)
	if err != nil {
		t.Fatal(err)
	}
	return st
}
