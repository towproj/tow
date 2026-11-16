package k3s

import (
	"strings"
	"testing"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/providers"
)

const header = "apiVersion: towproj.github.io/v1alpha1\nkind: Cluster\nmetadata: {name: x}\n"

func parse(t *testing.T, spec string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(header + spec + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func render(flags []providers.Flag) string {
	var out []string
	for _, f := range flags {
		s := f.Name + "=" + f.Value
		if f.Env != "" {
			s += "$" + f.Env
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

// The file's snapshot schedule is k3s's own; without one, k3s keeps its defaults.
func TestBackupsFlags(t *testing.T) {
	machines := "infrastructure: {provider: byoh, access: {ssh: {user: pi}}}, controlPlane: {machines: [{host: 10.0.0.1}]}"
	got := render(ServerFlags(parse(t, "spec: {kubernetes: {distribution: k3s, backups: {schedule: \"0 */6 * * *\", keep: 10}}, "+machines+"}")))
	if !strings.Contains(got, "etcd-snapshot-schedule-cron=0 */6 * * * etcd-snapshot-retention=10") {
		t.Errorf("flags %s", got)
	}
	if got := render(ServerFlags(parse(t, "spec: {kubernetes: {distribution: k3s}, "+machines+"}"))); strings.Contains(got, "etcd-snapshot") {
		t.Errorf("no schedule in the file, none in k3s's flags: %s", got)
	}
}

// A CNI that replaces kube-proxy turns k3s's off; otherwise it stays.
func TestKubeProxyReplaced(t *testing.T) {
	machines := "infrastructure: {provider: byoh, access: {ssh: {user: pi}}}, controlPlane: {machines: [{host: 10.0.0.1}]}"
	for spec, want := range map[string]bool{
		"{kubernetes: {distribution: k3s, networking: {cni: cilium, cilium: {kubeProxyReplacement: true}}}, " + machines + "}":  true,
		"{kubernetes: {distribution: k3s, networking: {cni: cilium, cilium: {kubeProxyReplacement: false}}}, " + machines + "}": false,
		"{kubernetes: {distribution: k3s, networking: {cni: cilium}}, " + machines + "}":                                        false,
		"{kubernetes: {distribution: k3s, networking: {cni: calico, calico: {dataplane: ebpf}}}, " + machines + "}":             true,
		"{kubernetes: {distribution: k3s, networking: {cni: calico}}, " + machines + "}":                                        false,
	} {
		got := strings.Contains(render(ServerFlags(parse(t, "spec: "+spec))), "disable-kube-proxy=true")
		if got != want {
			t.Errorf("%s: disable-kube-proxy = %v", spec, got)
		}
	}
}

// The local flavor is k3s as it ships: no flag at all.
func TestAsItShips(t *testing.T) {
	if got := render(ServerFlags(parse(t, "spec: {flavor: local}"))); got != "" {
		t.Errorf("flags = %q, want none", got)
	}
}

// Without a flavor, everything k3s bundles is turned off; a local cluster keeps flannel
// unless the file says cni: none.
func TestBare(t *testing.T) {
	extras := "disable=traefik disable=servicelb disable=local-storage disable=metrics-server disable=gateway-api-crd"
	if got := render(ServerFlags(parse(t, "spec: {infrastructure: {provider: local}}"))); got != extras {
		t.Errorf("flags =\n%s\nwant\n%s", got, extras)
	}
	got := render(ServerFlags(parse(t, "spec: {infrastructure: {provider: local}, kubernetes: {networking: {cni: none}}}")))
	if want := "flannel-backend=none disable-network-policy=true " + extras; got != want {
		t.Errorf("flags =\n%s\nwant\n%s", got, want)
	}
}

func TestEveryField(t *testing.T) {
	c := parse(t, `spec:
  kubernetes:
    distribution: k3s
    apiEndpoint: 192.168.1.100:6443
    apiServer: {certSANs: [k8s.lab], extraArgs: {audit-log-maxage: 30, service-account-issuer: [https://a, https://b]}}
    controllerManager: {extraArgs: {node-monitor-grace-period: 40s}}
    scheduler: {extraArgs: {v: 2}}
    etcd: {extraArgs: {heartbeat-interval: 500}}
    kubelet: {extraArgs: {max-pods: 150}}
    k3s:
      server: {extraArgs: {secrets-encryption: true, token: {env: K3S_TOKEN}}}
      agent: {extraArgs: {flannel-iface: eth1}}
    networking: {podCIDR: 10.50.0.0/16, clusterDomain: lab.local, cni: flannel, flannel: {backend: wireguard}}
    storage: {csi: [local-path]}
    ingress: {controllers: [traefik]}
    serviceLoadBalancer: {provider: servicelb}
    metricsServer: {enabled: true}
  infrastructure: {provider: byoh, access: {ssh: {user: u}}}
  controlPlane:
    schedulable: false
    labels: {zone: a, node-role.kubernetes.io/infra: ""}
    machines: [{host: 192.168.1.10}, {host: 192.168.1.11}, {host: 192.168.1.12}]
  workers:
    - name: gpu
      labels: {gpu: "true"}
      taints: [{key: gpu, value: "true", effect: NoSchedule}, {key: dedicated, effect: NoExecute}]
      kubelet: {extraArgs: {max-pods: 250}}
      machines: [{host: 192.168.1.20}]`)

	server := render(ServerFlags(c))
	want := "cluster-cidr=10.50.0.0/16 cluster-domain=lab.local flannel-backend=wireguard-native tls-san=k8s.lab " +
		"kube-apiserver-arg=audit-log-maxage=30 kube-apiserver-arg=service-account-issuer=https://a kube-apiserver-arg=service-account-issuer=https://b " +
		"kube-controller-manager-arg=node-monitor-grace-period=40s kube-scheduler-arg=v=2 etcd-arg=heartbeat-interval=500 " +
		"kubelet-arg=max-pods=150 node-label=zone=a node-taint=node-role.kubernetes.io/control-plane:NoSchedule " +
		"secrets-encryption=true token=$K3S_TOKEN"
	if server != want {
		t.Errorf("server flags =\n%s\nwant\n%s", server, want)
	}
	if strings.Contains(server, "node-role.kubernetes.io/infra") {
		t.Error("role labels go through the API, not the kubelet")
	}

	agent := render(AgentFlags(c, &c.Spec.Workers[0]))
	want = "kubelet-arg=max-pods=250 node-label=gpu=true node-taint=gpu=true:NoSchedule node-taint=dedicated:NoExecute flannel-iface=eth1"
	if agent != want {
		t.Errorf("agent flags =\n%s\nwant\n%s", agent, want)
	}
}
