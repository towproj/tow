package addons

import (
	"strings"
	"testing"

	"github.com/towproj/tow/internal/config"
	"gopkg.in/yaml.v3"
)

func lbOptions(lb config.ServiceLoadBalancer, apiKubeVIP bool) Options {
	k := config.Kubernetes{Distribution: config.DistributionKubeadm, ServiceLoadBalancer: lb}
	if apiKubeVIP {
		k.APILoadBalancer = config.APILBKubeVIP
	}
	return Options{Kubectl: "/bin/kubectl", Helm: "/bin/helm", Kubeconfig: "/state/kc", Kubernetes: k}
}

// docs decodes a multi-document manifest, failing on bad YAML.
func docs(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := yaml.NewDecoder(strings.NewReader(s))
	for {
		var d map[string]any
		if err := dec.Decode(&d); err != nil {
			if err.Error() == "EOF" {
				return out
			}
			t.Fatalf("bad YAML: %v\n%s", err, s)
		}
		out = append(out, d)
	}
}

func kinds(ds []map[string]any) string {
	var k []string
	for _, d := range ds {
		k = append(k, d["kind"].(string))
	}
	return strings.Join(k, ",")
}

func TestMetalLB(t *testing.T) {
	pool := config.AddressPool{Mode: config.ModeL2, Addresses: []string{"192.168.1.200-192.168.1.210", "192.168.1.240/28", "192.168.1.250"}}
	o := lbOptions(config.ServiceLoadBalancer{Provider: config.ServiceLBMetalLB, MetalLB: &config.PoolController{AddressPool: pool}}, false)
	c := o.serviceLB()
	if len(c) != 1 || c[0].name != "metallb" || c[0].version != MetalLBVersion || len(c[0].steps) != 3 {
		t.Fatalf("%+v", c)
	}
	ns, chart, objects := c[0].steps[0], c[0].steps[1], c[0].steps[2]
	if !strings.Contains(ns.stdin, "pod-security.kubernetes.io/enforce: privileged") || !strings.Contains(strings.Join(chart.args, " "), "upgrade --install metallb metallb --repo https://metallb.github.io/metallb --version 0.16.2 --namespace metallb-system") {
		t.Errorf("namespace %q, chart %v", ns.stdin, chart.args)
	}
	d := docs(t, objects.stdin)
	if kinds(d) != "IPAddressPool,L2Advertisement" || !strings.Contains(objects.stdin, "addresses: [192.168.1.200-192.168.1.210, 192.168.1.240/28, 192.168.1.250/32]") {
		t.Errorf("objects:\n%s", objects.stdin)
	}

	pool.Mode, pool.BGP = config.ModeBGP, &config.BGP{LocalASN: 64512, Peers: []config.BGPPeer{{Address: "192.168.1.1", ASN: 64513}, {Address: "192.168.1.2", ASN: 64513}}}
	o = lbOptions(config.ServiceLoadBalancer{Provider: config.ServiceLBMetalLB, MetalLB: &config.PoolController{AddressPool: pool}}, false)
	objects = o.serviceLB()[0].steps[2]
	if d := docs(t, objects.stdin); kinds(d) != "IPAddressPool,BGPPeer,BGPPeer,BGPAdvertisement" || d[1]["apiVersion"] != "metallb.io/v1beta2" ||
		!strings.Contains(objects.stdin, "spec: {myASN: 64512, peerASN: 64513, peerAddress: 192.168.1.2}") {
		t.Errorf("BGP objects:\n%s", objects.stdin)
	}
}

func TestKubeVIPServices(t *testing.T) {
	pool := &config.PoolController{AddressPool: config.AddressPool{Mode: config.ModeL2, Addresses: []string{"192.168.1.200-192.168.1.210", "192.168.1.240/28"}}}
	lb := config.ServiceLoadBalancer{Provider: config.ServiceLBKubeVIP, KubeVIP: pool}

	// The API's kube-vip announces Services: only the cloud provider and its ranges here.
	c := lbOptions(lb, true).serviceLB()[0]
	var cmds []string
	for _, s := range c.steps {
		cmds = append(cmds, s.args[0]+" "+s.args[len(s.args)-1])
	}
	if c.name != "kube-vip-cloud-provider" || len(c.steps) != 3 || !strings.HasSuffix(c.steps[0].args[len(c.steps[0].args)-1], "/kube-vip-cloud-provider/v0.0.13/manifest/kube-vip-cloud-controller.yaml") ||
		!strings.Contains(c.steps[1].stdin, `range-global: "192.168.1.200-192.168.1.210,192.168.1.240-192.168.1.255"`) {
		t.Errorf("with the API's kube-vip: %v\n%s", cmds, c.steps[1].stdin)
	}

	// Without it: a DaemonSet of its own, waited for.
	c = lbOptions(lb, false).serviceLB()[0]
	if len(c.steps) != 5 || !strings.Contains(c.steps[2].stdin, "name: kube-vip-svc") || !strings.Contains(strings.Join(c.steps[4].args, " "), "rollout status daemonset/kube-vip-svc") {
		t.Errorf("services-only: %d steps", len(c.steps))
	}
}

func TestCiliumServiceLB(t *testing.T) {
	pool := &config.AddressPool{Mode: config.ModeL2, Addresses: []string{"192.168.1.200-192.168.1.210", "192.168.1.240/28"}}
	lb := config.ServiceLoadBalancer{Provider: config.ServiceLBCilium, Cilium: pool}
	objects := lbOptions(lb, false).serviceLB()[0].steps[0].stdin
	d := docs(t, objects)
	if kinds(d) != "CiliumLoadBalancerIPPool,CiliumL2AnnouncementPolicy" || !strings.Contains(objects, "- start: \"192.168.1.200\"\n    stop: \"192.168.1.210\"") ||
		!strings.Contains(objects, `- cidr: "192.168.1.240/28"`) || d[1]["apiVersion"] != "cilium.io/v2alpha1" {
		t.Errorf("L2:\n%s", objects)
	}
	if v := CiliumServiceLBValues(lb); at(v, "l2announcements", "enabled") != true {
		t.Errorf("values %v", v)
	}

	pool.Mode, pool.BGP = config.ModeBGP, &config.BGP{LocalASN: 64512, Peers: []config.BGPPeer{{Address: "192.168.1.1", ASN: 64513}}}
	objects = lbOptions(lb, false).serviceLB()[0].steps[0].stdin
	if d := docs(t, objects); kinds(d) != "CiliumLoadBalancerIPPool,CiliumBGPClusterConfig,CiliumBGPPeerConfig,CiliumBGPAdvertisement" {
		t.Errorf("BGP:\n%s", objects)
	}
	if v := CiliumServiceLBValues(lb); at(v, "bgpControlPlane", "enabled") != true {
		t.Errorf("values %v", v)
	}
	if CiliumServiceLBValues(config.ServiceLoadBalancer{Provider: config.ServiceLBMetalLB}) != nil {
		t.Error("only for Cilium's own")
	}
}

func TestBounds(t *testing.T) {
	for in, want := range map[string]string{"10.0.0.5-10.0.0.9": "10.0.0.5-10.0.0.9", "10.0.0.0/30": "10.0.0.0-10.0.0.3", "10.0.0.7": "10.0.0.7-10.0.0.7", "10.0.0.9/29": "10.0.0.8-10.0.0.15"} {
		if a, b := bounds(in); a+"-"+b != want {
			t.Errorf("%s: %s-%s", in, a, b)
		}
	}
}
