package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	onAWS  = "infrastructure: {provider: aws, aws: {region: eu-west-3}}"
	onBYOH = "infrastructure: {provider: byoh, access: {ssh: {user: u}}}, controlPlane: {machines: [{host: 192.168.1.10}]}"
	// onBYOHBlock is onBYOH for block-style specs.
	onBYOHBlock = "infrastructure: {provider: byoh, access: {ssh: {user: u}}}\n  controlPlane: {machines: [{host: 192.168.1.10}]}"
	onProxmox   = `infrastructure: {provider: proxmox, proxmox: {url: "https://192.168.100.10:8006", node: pve, template: 9000},
    network: {subnet: 192.168.100.0/24, addresses: 192.168.100.20-192.168.100.29, gateway: 192.168.100.1, dns: [1.1.1.1]}}`
)

// expectErrors parses spec and checks every wanted message is in the error.
func expectErrors(t *testing.T, spec string, want ...string) {
	t.Helper()
	_, err := Parse([]byte(header + spec + "\n"))
	if err == nil {
		t.Fatalf("expected an error for:\n%s", spec)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error missing %q:\n%v", w, err)
		}
	}
}

// Presets fill in what's installed; local and homelab are k3d and k3s as they ship.
func TestPresets(t *testing.T) {
	tests := []struct {
		flavor, provider string
		cni              string
		csi, ingress     []string
		serviceLB        string
		schedulable      string
	}{
		{FlavorLocal, "", CNIFlannel, []string{CSILocalPath}, []string{IngressTraefik}, ServiceLBServiceLB, SchedulableTrue},
		{FlavorHomelab, "infrastructure: {provider: byoh, access: {ssh: {user: u}}}, controlPlane: {machines: [{host: 10.0.0.1}]}",
			CNIFlannel, []string{CSILocalPath}, []string{IngressTraefik}, ServiceLBServiceLB, SchedulableTrue},
		{FlavorDevTest, onAWS, CNICalico, []string{CSILocalPath}, []string{IngressTraefik}, ServiceLBNone, SchedulableAuto},
		{FlavorProduction, onAWS, CNICilium, []string{CSILonghorn}, []string{IngressTraefik}, ServiceLBNone, SchedulableAuto},
	}
	for _, tt := range tests {
		t.Run(tt.flavor, func(t *testing.T) {
			spec := "spec: {flavor: " + tt.flavor
			if tt.provider != "" {
				spec += ", " + tt.provider
			}
			c := mustParse(t, header+spec+"}\n")
			k := c.Spec.Kubernetes
			if k.Networking.CNI != tt.cni || !slices.Equal(k.Storage.CSI, tt.csi) || !slices.Equal(k.Ingress.Controllers, tt.ingress) ||
				k.ServiceLoadBalancer.Provider != tt.serviceLB || !*k.MetricsServer.Enabled || c.Spec.ControlPlane.Schedulable != tt.schedulable {
				t.Errorf("preset: cni %s, csi %v, ingress %v, lb %s, metrics %v, schedulable %s",
					k.Networking.CNI, k.Storage.CSI, k.Ingress.Controllers, k.ServiceLoadBalancer.Provider, *k.MetricsServer.Enabled, c.Spec.ControlPlane.Schedulable)
			}
			if k.Storage.DefaultClass != tt.csi[0] || k.Ingress.DefaultClass != tt.ingress[0] {
				t.Errorf("single choices should be the default classes: %q, %q", k.Storage.DefaultClass, k.Ingress.DefaultClass)
			}
			if k.Kubeconfig.Context != "x" || !*k.Kubeconfig.Merge || k.Networking.ClusterDomain != DefaultClusterDomain {
				t.Errorf("kubeconfig %+v, domain %s", k.Kubeconfig, k.Networking.ClusterDomain)
			}
		})
	}
}

// Without a flavor you get Kubernetes with none of the optional components the file doesn't
// name, not even a CNI; local clusters are the exception for the CNI only (flannel).
func TestNoAddOnsWithoutFlavor(t *testing.T) {
	for _, spec := range []string{
		"spec: {kubernetes: {distribution: kubeadm}, " + onAWS + "}",
		"spec: {kubernetes: {distribution: k3s}, " + onBYOH + "}",
		"spec: {infrastructure: {provider: local}, kubernetes: {networking: {cni: none}}}",
	} {
		c := mustParse(t, header+spec+"\n")
		k := c.Spec.Kubernetes
		if k.Networking.CNI != CNINone || len(k.Storage.CSI) != 0 || len(k.Ingress.Controllers) != 0 ||
			k.ServiceLoadBalancer.Provider != ServiceLBNone || *k.MetricsServer.Enabled || c.Spec.ControlPlane.Schedulable != SchedulableAuto {
			t.Errorf("%s: something was added: %+v", spec, k)
		}
		w := strings.Join(c.Warnings(), "\n")
		if !strings.Contains(w, "no CNI: nodes stay NotReady until one is installed\n  → set spec.kubernetes.networking.cni (") ||
			!strings.Contains(w, "or install your own with spec.kubernetes.addons") {
			t.Errorf("%s: no-CNI warning missing: %q", spec, w)
		}
	}
	// The recommendation lists only what the distribution can install.
	c := mustParse(t, header+"spec: {infrastructure: {provider: local}, kubernetes: {networking: {cni: none}}}\n")
	if w := strings.Join(c.Warnings(), "\n"); !strings.Contains(w, "networking.cni (flannel)") {
		t.Errorf("local clusters take flannel only: %q", w)
	}

	// A local cluster without a flavor still gets a CNI, and nothing else.
	c = mustParse(t, header+"spec: {infrastructure: {provider: local}}\n")
	k := c.Spec.Kubernetes
	if k.Networking.CNI != CNIFlannel || len(k.Storage.CSI) != 0 || len(k.Ingress.Controllers) != 0 ||
		k.ServiceLoadBalancer.Provider != ServiceLBNone || *k.MetricsServer.Enabled || len(c.Warnings()) != 0 {
		t.Errorf("bare local cluster: %+v, warnings %v", k, c.Warnings())
	}
}

// What the file says replaces the preset's choice, including "none" and empty lists.
func TestFileOverridesPreset(t *testing.T) {
	c := mustParse(t, header+`spec:
  flavor: homelab
  kubernetes:
    networking: {cni: cilium, cilium: {kubeProxyReplacement: true}}
    storage: {csi: []}
    ingress: {controllers: [cilium]}
    serviceLoadBalancer: {provider: cilium, cilium: {addresses: [192.168.1.200-192.168.1.210]}}
    metricsServer: {enabled: false}
  infrastructure: {provider: byoh, access: {ssh: {user: u}}}
  controlPlane: {schedulable: auto, machines: [{host: 192.168.1.10}]}
`)
	k := c.Spec.Kubernetes
	if k.Networking.CNI != CNICilium || len(k.Storage.CSI) != 0 || k.Storage.DefaultClass != "" ||
		!slices.Equal(k.Ingress.Controllers, []string{IngressCilium}) || k.Ingress.DefaultClass != IngressCilium ||
		k.ServiceLoadBalancer.Provider != ServiceLBCilium || *k.MetricsServer.Enabled || c.Spec.ControlPlane.Schedulable != SchedulableAuto {
		t.Errorf("overrides lost: %+v, schedulable %s", k, c.Spec.ControlPlane.Schedulable)
	}
}

// Every field at once, on byoh with kubeadm and on k3s: nothing valid is refused.
func TestEverythingAccepted(t *testing.T) {
	c := mustParse(t, header+`spec:
  kubernetes:
    distribution: kubeadm
    version: v1.37.2
    containerRuntime: containerd
    containerd: {version: 2.4.2}
    apiEndpoint: 192.168.1.100:6443
    apiLoadBalancer: kube-vip
    apiServer:
      certSANs: [k8s.lab.example.com, "*.lab.example.com", 203.0.113.7]
      extraArgs:
        audit-log-maxage: 30
        service-account-issuer: [https://new.example, https://old.example]
        tls-private-key-file: /etc/kubernetes/tls.key
    controllerManager: {extraArgs: {node-monitor-grace-period: 40s}}
    scheduler: {extraArgs: {v: 2}}
    etcd: {extraArgs: {quota-backend-bytes: "8589934592"}}
    kubelet: {extraArgs: {max-pods: 150}}
    networking:
      podCIDR: 10.244.0.0/16
      serviceCIDR: 10.96.0.0/12
      clusterDomain: lab.local
      cni: cilium
      cilium: {version: 1.18.4, kubeProxyReplacement: true, encryption: wireguard}
    storage:
      csi: [longhorn, nfs, smb, local-path, openebs, rook-ceph]
      defaultClass: longhorn
      longhorn: {version: 1.10.1, replicas: 2}
      nfs: {server: nas.lan, path: /export/k8s}
      smb: {source: //nas/k8s, usernameEnv: SMB_USER, passwordEnv: SMB_PASSWORD}
      local-path: {version: v0.0.32}
    serviceLoadBalancer:
      provider: cilium
      cilium: {addresses: [192.168.1.200-192.168.1.220, 192.168.1.240/28]}
    ingress:
      controllers: [traefik, cilium]
      defaultClass: traefik
      traefik: {version: 37.4.0}
    metricsServer: {enabled: true, version: 3.13.0}
    addons:
      helmCharts:
        - {name: cert-manager, repo: https://charts.jetstack.io, chart: cert-manager, version: v1.19.1,
           namespace: cert-manager, values: {crds: {enabled: true}}}
        - {name: podinfo, repo: oci://ghcr.io/stefanprodan/charts, chart: podinfo, version: 6.9.2}
      manifests: [https://example.com/app.yaml]
    kubeconfig: {context: lab, merge: false}
    backups: {schedule: "30 2 * * 1-5", keep: 14}
  infrastructure: {provider: byoh, access: {ssh: {user: u}}}
  controlPlane:
    schedulable: false
    labels: {zone: rack-1, topology.kubernetes.io/zone: a, node.kubernetes.io/pool: cp}
    taints: [{key: dedicated, value: control, effect: NoSchedule}]
    machines: [{host: 192.168.1.10}, {host: 192.168.1.11}, {host: 192.168.1.12}]
  workers:
    - name: gpu
      labels: {example.com/gpu: "true"}
      taints: [{key: gpu, value: "true", effect: NoSchedule}, {key: gpu, value: "true", effect: NoExecute}]
      kubelet: {extraArgs: {max-pods: 250}}
      machines: [{host: 192.168.1.20}]
`)
	if len(c.Warnings()) != 0 {
		t.Errorf("a complete file deserves no warning: %v", c.Warnings())
	}
	if h := c.Spec.Kubernetes.Addons.HelmCharts[1]; h.Namespace != h.Name {
		t.Errorf("a chart without namespace gets one named after its release, got %q", h.Namespace)
	}

	mustParse(t, header+`spec:
  kubernetes:
    distribution: k3s
    apiEndpoint: 192.168.1.100:6443
    etcd: {extraArgs: {heartbeat-interval: 500}}
    k3s:
      server: {extraArgs: {secrets-encryption: true, etcd-snapshot-compress: true}}
      agent: {extraArgs: {flannel-iface: eth1}}
    backups: {schedule: "0 */6 * * *", keep: 10}
    networking: {cni: flannel, flannel: {backend: wireguard}}
    storage: {csi: [local-path]}
    ingress: {controllers: [traefik]}
    serviceLoadBalancer: {provider: servicelb}
    metricsServer: {enabled: true}
  infrastructure: {provider: byoh, access: {ssh: {user: u}}}
  controlPlane: {machines: [{host: 192.168.1.10}, {host: 192.168.1.11}, {host: 192.168.1.12}]}
`)
}

func TestNetworkingRefusals(t *testing.T) {
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm, networking: {cni: flannel, cilium: {version: 1.18.4}}}, "+onAWS+"}",
		"spec.kubernetes.networking.cilium is set but cni is flannel")
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm, networking: {cni: none, calico: {}}}, "+onAWS+"}",
		"spec.kubernetes.networking.calico is set but cni is none")
	expectErrors(t, "spec: {flavor: local, kubernetes: {networking: {cni: calico}}}",
		"spec.kubernetes.networking.cni: calico isn't available on local clusters (they take flannel or none)")
	expectErrors(t, "spec: {kubernetes: {distribution: k3s, networking: {cni: flannel, flannel: {version: 0.27.0}}}, "+onAWS+"}",
		"spec.kubernetes.networking.flannel.version: flannel comes bundled with k3s, so its version follows k3s's")
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm, networking: {cni: flannel, flannel: {backend: udp}}}, "+onAWS+"}",
		`spec.kubernetes.networking.flannel.backend "udp" must be vxlan, host-gw or wireguard`)
	expectErrors(t, `spec: {kubernetes: {distribution: kubeadm, networking: {cni: cilium, cilium: {version: "1.18", encryption: ipsec}}}, `+onAWS+"}",
		`spec.kubernetes.networking.cilium.version "1.18" must be an exact release`, `cilium.encryption "ipsec" must be wireguard or none`)
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm, networking: {cni: calico, calico: {encapsulation: gre}}}, "+onAWS+"}",
		`calico.encapsulation "gre" must be vxlan, vxlan-cross-subnet, ipip, ipip-cross-subnet or none`)
	expectErrors(t, "spec: {kubernetes: {distribution: k3s, networking: {cni: calico, calico: {dataplane: vpp}}}, "+onAWS+"}",
		`calico.dataplane "vpp" must be iptables or ebpf`)
	expectErrors(t, "spec: {kubernetes: {distribution: k3s, networking: {cni: calico, calico: {dataplane: ebpf, encapsulation: ipip-cross-subnet}}}, "+onAWS+"}",
		`calico.encapsulation ipip-cross-subnet doesn't work with dataplane ebpf (Calico docs: "IPIP is not supported")`)
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm, networking: {clusterDomain: Cluster.Local}}, "+onAWS+"}",
		`spec.kubernetes.networking.clusterDomain "Cluster.Local" must be a lowercase DNS name`)
}

func TestStorageRefusals(t *testing.T) {
	expectErrors(t, `spec:
  kubernetes:
    distribution: kubeadm
    storage: {csi: [longhorn, longhorn, aws-ebs, proxmox, nfs, smb, ceph], defaultClass: openebs, local-path: {}}
  `+onAWS,
		"spec.kubernetes.storage.csi[1]: longhorn is listed twice",
		"spec.kubernetes.storage.csi[3]: proxmox works only on provider proxmox (it uses Proxmox disks)",
		`spec.kubernetes.storage.csi[6]: "ceph" is unknown (aws-ebs, aws-efs, azure-disk, gcp-pd, local-path, longhorn, nfs, openebs, proxmox, rook-ceph or smb)`,
		"spec.kubernetes.storage.nfs is required with csi nfs: the server and path of the export",
		"spec.kubernetes.storage.smb is required with csi smb",
		`spec.kubernetes.storage.defaultClass "openebs" must be one of storage.csi`,
		"spec.kubernetes.storage.local-path is set but storage.csi doesn't list it")
	expectErrors(t, `spec:
  kubernetes:
    distribution: kubeadm
    storage:
      csi: [nfs, smb, longhorn]
      defaultClass: nfs
      nfs: {server: "bad host!", path: export}
      smb: {source: nas/k8s, usernameEnv: SMB_USER, passwordEnv: 1bad}
      longhorn: {replicas: 0, version: latest}
  `+onAWS,
		`spec.kubernetes.storage.nfs.server "bad host!" must be the NFS server's address`,
		`spec.kubernetes.storage.nfs.path "export" must be the exported directory`,
		`spec.kubernetes.storage.smb.source "nas/k8s" must be a share like //nas/k8s`,
		`spec.kubernetes.storage.smb.passwordEnv "1bad" must name an environment variable`,
		"spec.kubernetes.storage.longhorn.replicas must be at least 1",
		`spec.kubernetes.storage.longhorn.version "latest" must be an exact release`)
	expectErrors(t, "spec: {flavor: local, kubernetes: {storage: {csi: [longhorn]}}}",
		"spec.kubernetes.storage.csi[0]: longhorn isn't available on local clusters (they take local-path)")
	expectErrors(t, "spec: {kubernetes: {distribution: k3s, storage: {csi: [local-path], local-path: {version: v0.0.32}}}, "+onAWS+"}",
		"local-path comes bundled with k3s")
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm, storage: {csi: [gcp-pd]}}, "+onAWS+"}",
		"spec.kubernetes.storage.csi[0]: gcp-pd works only on provider gcp (it uses Compute Engine persistent disks)")
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm, storage: {csi: [gcp-pd], gcp-pd: {version: latest}}}, infrastructure: {provider: gcp, gcp: {project: my-project, region: europe-west1}}}",
		`spec.kubernetes.storage.gcp-pd.version "latest" must be an exact release`)
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm, storage: {csi: [azure-disk]}}, infrastructure: {provider: gcp, gcp: {project: my-project, region: europe-west1}}}",
		"spec.kubernetes.storage.csi[0]: azure-disk works only on provider azure (it uses Azure managed disks)")
}

func TestServiceLBRefusals(t *testing.T) {
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm, serviceLoadBalancer: {provider: servicelb, metallb: {addresses: [10.0.0.1]}}}, "+onAWS+"}",
		"spec.kubernetes.serviceLoadBalancer.provider: servicelb needs k3s or k3d (it comes bundled with k3s)",
		"spec.kubernetes.serviceLoadBalancer.metallb is set but provider is servicelb")
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm, networking: {cni: calico}, serviceLoadBalancer: {provider: cilium}}, "+onAWS+"}",
		"spec.kubernetes.serviceLoadBalancer.provider: cilium needs cni: cilium",
		"spec.kubernetes.serviceLoadBalancer.cilium is required with provider cilium")
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm, serviceLoadBalancer: {provider: metallb}}, "+onAWS+"}",
		"spec.kubernetes.serviceLoadBalancer.metallb is required with provider metallb: the addresses it hands out")
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm, serviceLoadBalancer: {provider: aws}}, "+onProxmox+"}",
		"spec.kubernetes.serviceLoadBalancer.provider: aws works only on provider aws (it creates AWS load balancers)")
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm, serviceLoadBalancer: {provider: metallb, metallb: {addresses: [10.200.0.0/28]}}}, "+onAWS+"}",
		"spec.kubernetes.serviceLoadBalancer.metallb.mode l2 can't work on aws: a VPC ignores layer-2 announcements")
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm, serviceLoadBalancer: {provider: metallb, metallb: {addresses: [10.200.0.0/28]}}}, infrastructure: {provider: gcp, gcp: {project: my-project, region: europe-west1}}}",
		"spec.kubernetes.serviceLoadBalancer.metallb.mode l2 can't work on gcp: a VPC ignores layer-2 announcements")
	expectErrors(t, `spec:
  kubernetes:
    distribution: kubeadm
    serviceLoadBalancer: {provider: kube-vip, kube-vip: {mode: bgp, addresses: [192.168.1.200/29], bgp: {localASN: 64512}}}
  `+onBYOHBlock,
		"spec.kubernetes.serviceLoadBalancer.kube-vip.bgp is required with mode bgp: localASN and at least one peer")
	expectErrors(t, `spec:
  kubernetes:
    distribution: kubeadm
    serviceLoadBalancer: {provider: metallb, metallb: {mode: l2, addresses: [192.168.1.200/29], bgp: {localASN: 64512}}}
  `+onBYOHBlock,
		"spec.kubernetes.serviceLoadBalancer.metallb.bgp is set but mode is l2")
	expectErrors(t, `spec:
  kubernetes:
    distribution: kubeadm
    serviceLoadBalancer: {provider: metallb, metallb: {mode: bgp, addresses: [192.168.1.200/29], bgp: {localASN: 64512, peers: [{address: router}]}}}
  `+onBYOHBlock,
		`spec.kubernetes.serviceLoadBalancer.metallb.bgp.peers[0].address "router" must be an IPv4 address`,
		"spec.kubernetes.serviceLoadBalancer.metallb.bgp.peers[0].asn is required")
}

// Pool addresses must be free: not the machines', the API's, the cluster's ranges, or each other's.
func TestAddressPools(t *testing.T) {
	expectErrors(t, `spec:
  kubernetes:
    distribution: k3s
    apiEndpoint: 192.168.1.100:6443
    serviceLoadBalancer:
      provider: metallb
      metallb: {addresses: [192.168.1.5-192.168.1.20, 192.168.1.100/32, 10.42.0.0/24, 192.168.1.15, lots]}
  `+onBYOHBlock,
		"metallb.addresses[0] 192.168.1.5-192.168.1.20 overlaps machine 192.168.1.10",
		"metallb.addresses[1] 192.168.1.100/32 overlaps spec.kubernetes.apiEndpoint",
		"metallb.addresses[2] 10.42.0.0/24 overlaps spec.kubernetes.networking.podCIDR",
		"metallb.addresses[3] 192.168.1.15 overlaps spec.kubernetes.serviceLoadBalancer.metallb.addresses[0]",
		`metallb.addresses[4] "lots" must be a range like 192.168.1.200-192.168.1.220 or 192.168.1.240/28`)
	expectErrors(t, `spec:
  kubernetes:
    distribution: kubeadm
    serviceLoadBalancer: {provider: metallb, metallb: {addresses: [192.168.100.25-192.168.100.40, 192.168.200.0/28, 192.168.100.1]}}
  `+onProxmox,
		"metallb.addresses[0] 192.168.100.25-192.168.100.40 overlaps spec.infrastructure.network.addresses (the VMs)",
		"metallb.addresses[1] 192.168.200.0/28 must be inside spec.infrastructure.network.subnet (192.168.100.0/24): layer-2 announcements reach only the local network",
		"metallb.addresses[2] 192.168.100.1 overlaps spec.infrastructure.network.gateway")

	// BGP pools may live outside the machines' subnet; mode follows the bgp block when unset.
	c := mustParse(t, header+`spec:
  kubernetes:
    distribution: kubeadm
    serviceLoadBalancer: {provider: metallb, metallb: {addresses: [10.200.0.0/28], bgp: {localASN: 64512, peers: [{address: 192.168.100.1, asn: 64513}]}}}
  `+onProxmox+"\n")
	if m := c.Spec.Kubernetes.ServiceLoadBalancer.MetalLB.Mode; m != ModeBGP {
		t.Errorf("mode = %q, want bgp when a bgp block is given", m)
	}
	c = mustParse(t, header+"spec: {kubernetes: {distribution: kubeadm, serviceLoadBalancer: {provider: metallb, metallb: {addresses: [192.168.100.200/29]}}}, "+onProxmox+"}\n")
	if m := c.Spec.Kubernetes.ServiceLoadBalancer.MetalLB.Mode; m != ModeL2 {
		t.Errorf("mode = %q, want l2 by default", m)
	}
}

func TestIngressRefusals(t *testing.T) {
	expectErrors(t, `spec:
  kubernetes:
    distribution: kubeadm
    networking: {cni: calico}
    ingress: {controllers: [traefik, traefik, cilium, envoy-gateway, istio], defaultClass: cilium}
  `+onAWS,
		"spec.kubernetes.ingress.controllers[1]: traefik is listed twice",
		"spec.kubernetes.ingress.controllers[2]: cilium needs cni: cilium",
		"spec.kubernetes.ingress.controllers[3]: Envoy Gateway installs as a chart of its own, which tow has nothing to add to: put it in spec.kubernetes.addons.helmCharts",
		`spec.kubernetes.ingress.controllers[4]: "istio" is unknown`)
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm, ingress: {controllers: [nginx], defaultClass: traefik}}, "+onAWS+"}",
		"controllers[0]: F5 NGINX Ingress Controller installs as a chart of its own",
		`spec.kubernetes.ingress.defaultClass "traefik" must be one of ingress.controllers`)
	expectErrors(t, "spec: {kubernetes: {distribution: k3s, ingress: {controllers: [traefik], traefik: {version: 37.4.0}}}, "+onAWS+"}",
		"spec.kubernetes.ingress.traefik.version: traefik comes bundled with k3s")
	// Cilium's ingress goes through its kube-proxy replacement and its load balancer.
	expectErrors(t, "spec: {kubernetes: {distribution: k3s, networking: {cni: cilium}, ingress: {controllers: [cilium]}}, "+onBYOH+"}",
		"cilium needs spec.kubernetes.networking.cilium.kubeProxyReplacement: true",
		"cilium needs a service load balancer")
}

// The ports the machines' firewalls open to the admin addresses.
func TestAccessPorts(t *testing.T) {
	c := mustParse(t, header+"spec: {kubernetes: {distribution: kubeadm}, infrastructure: {provider: aws, aws: {region: eu-west-3}, access: {ports: [8080, 9000]}}}\n")
	if !slices.Equal(c.Spec.Infrastructure.Access.Ports, []int{8080, 9000}) {
		t.Errorf("ports %v", c.Spec.Infrastructure.Access.Ports)
	}
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm}, infrastructure: {provider: aws, aws: {region: eu-west-3}, access: {ports: [0, 8080, 8080, 70000]}}}",
		"access.ports[0] 0 must be a TCP port", "access.ports[2]: 8080 is listed twice", "access.ports[3] 70000 must be a TCP port")
	expectErrors(t, "spec: {flavor: local, infrastructure: {provider: local, access: {ports: [8080]}}}", "spec.infrastructure.access")
	// Your own machines take admin addresses too: ufw lets them in.
	mustParse(t, header+"spec: {kubernetes: {distribution: k3s}, infrastructure: {provider: byoh, access: {ssh: {user: u}, adminCIDRs: [192.168.1.0/24]}}, controlPlane: {machines: [{host: 192.168.1.10}]}}\n")
}

func TestAddonRefusals(t *testing.T) {
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm, metricsServer: {version: 3.13.0}}, "+onAWS+"}",
		"spec.kubernetes.metricsServer.version is set but metricsServer.enabled isn't true")
	expectErrors(t, "spec: {kubernetes: {distribution: k3s, metricsServer: {enabled: true, version: 3.13.0}}, "+onAWS+"}",
		"metrics-server comes bundled with k3s")
	expectErrors(t, `spec:
  kubernetes:
    distribution: kubeadm
    addons:
      helmCharts:
        - {name: Bad, repo: "http://charts.example", chart: "", version: "^1.0", namespace: NS}
        - {name: a, repo: "oci://ghcr.io/x", chart: c, version: 1.0.0}
        - {name: a, repo: "https://x.io", chart: c, version: 1.0.0}
        - {name: b, repo: "https://x.io", chart: c, version: 1.0.0, namespace: default}
      manifests: ["http://example.com/a.yaml", ./missing.yaml, "", {path: "https://x.io/a.yaml", namespace: default}, "https://x.io/_.yaml", {path: "https://x.io/a.yaml", namespace: Shop}]
  `+onAWS,
		`helmCharts[0].name "Bad" must be a lowercase DNS label of at most 53 characters`,
		`helmCharts[0].namespace "NS" must be a lowercase DNS label`,
		`helmCharts[0].repo "http://charts.example" must be a chart repository, https:// or oci://`,
		"helmCharts[0].chart is required",
		`helmCharts[0].version "^1.0" must be an exact chart version`,
		"helmCharts[2]: release a in namespace a is already installed by spec.kubernetes.addons.helmCharts[1]",
		`helmCharts[3].namespace: tow doesn't install into "default"`,
		`manifests[0] "http://example.com/a.yaml" must be an https:// URL or a file next to the cluster file`,
		"manifests[1] ./missing.yaml doesn't exist (local paths are relative to the cluster file)",
		"manifests[2] is empty",
		`manifests[3].namespace: tow doesn't install into "default"`,
		`manifests[4]: no namespace can be named after "https://x.io/_.yaml"; write it as {path: https://x.io/_.yaml, namespace: <name>}`,
		`manifests[5].namespace "Shop" must be a lowercase DNS label`)
}

// A manifest is a plain string or {path, namespace}; its objects that name no namespace go
// to its own, or one named after the file. The plain form keeps the fingerprint of before.
func TestManifestForms(t *testing.T) {
	c := mustParse(t, header+`spec:
  flavor: local
  kubernetes:
    addons:
      manifests:
        - https://example.com/deploy/My_App.yaml
        - {path: "https://example.com/crds.yaml", namespace: shop}
        - https://example.com/bundles/monitoring/
`)
	m := c.Spec.Kubernetes.Addons.Manifests
	if m[0].Path != "https://example.com/deploy/My_App.yaml" || m[0].Target() != "my-app" {
		t.Errorf("plain: %+v → %q", m[0], m[0].Target())
	}
	if m[1].Target() != "shop" || m[2].Target() != "monitoring" {
		t.Errorf("targets: %q %q", m[1].Target(), m[2].Target())
	}
	f := c.Fields()
	// Keyed by path, whichever way they're written.
	if f["spec.kubernetes.addons.manifests[https://example.com/deploy/My_App.yaml]"] != "https://example.com/deploy/My_App.yaml" ||
		f["spec.kubernetes.addons.manifests[https://example.com/crds.yaml].namespace"] != "shop" {
		t.Errorf("fields: %v", f)
	}
	if (Manifest{Path: "./manifests/app.yml"}).Target() != "app" || (Manifest{Path: "manifests"}).Target() != "manifests" {
		t.Error("local files and folders")
	}
}

// Local manifests resolve against the cluster file's folder, wherever apply runs from.
func TestManifestsRelativeToFile(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "manifests"), 0o755)
	os.WriteFile(filepath.Join(dir, "manifests", "app.yaml"), []byte("kind: List\n"), 0o600)
	file := filepath.Join(dir, "tow.yaml")
	os.WriteFile(file, []byte(header+"spec: {flavor: local, kubernetes: {addons: {manifests: [manifests/app.yaml, manifests]}}}\n"), 0o600)

	c, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.ManifestPath("manifests/app.yaml"), filepath.Join(dir, "manifests", "app.yaml"); got != want {
		t.Errorf("ManifestPath = %s, want %s", got, want)
	}
}

func TestAPILoadBalancer(t *testing.T) {
	ha := "controlPlane: {machines: [{host: 192.168.1.10}, {host: 192.168.1.11}, {host: 192.168.1.12}]}"
	c := mustParse(t, header+"spec: {kubernetes: {distribution: k3s, apiEndpoint: \"192.168.1.100:6443\"}, infrastructure: {provider: byoh, access: {ssh: {user: u}}}, "+ha+"}\n")
	if c.Spec.Kubernetes.APILoadBalancer != APILBKubeVIP {
		t.Errorf("byoh with apiEndpoint defaults to kube-vip, got %q", c.Spec.Kubernetes.APILoadBalancer)
	}
	c = mustParse(t, header+"spec: {kubernetes: {distribution: k3s, apiEndpoint: \"192.168.1.100:6443\", apiLoadBalancer: external}, infrastructure: {provider: byoh, access: {ssh: {user: u}}}, "+ha+"}\n")
	if c.Spec.Kubernetes.APILoadBalancer != APILBExternal {
		t.Errorf("the file's choice must win, got %q", c.Spec.Kubernetes.APILoadBalancer)
	}
	if c := mustParse(t, header+"spec: {kubernetes: {distribution: k3s}, "+onBYOH+"}\n"); c.Spec.Kubernetes.APILoadBalancer != "" {
		t.Errorf("no apiEndpoint, nothing to serve: got %q", c.Spec.Kubernetes.APILoadBalancer)
	}

	expectErrors(t, "spec: {kubernetes: {distribution: k3s, apiEndpoint: \"192.168.1.100:6443\", apiLoadBalancer: haproxy}, "+onBYOH+"}",
		`spec.kubernetes.apiLoadBalancer "haproxy" must be kube-vip or external`)
	expectErrors(t, "spec: {kubernetes: {distribution: k3s, apiLoadBalancer: external}, "+onBYOH+"}",
		"spec.kubernetes.apiLoadBalancer needs spec.kubernetes.apiEndpoint: the address it serves")
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm, apiEndpoint: \"192.168.100.100:6443\", apiLoadBalancer: external}, "+onProxmox+"}",
		"spec.kubernetes.apiLoadBalancer external isn't used on proxmox")

	// kube-vip holds an IPv4 address; the API servers answer on it at their own port. Your own
	// load balancer may be a name, on any port.
	expectErrors(t, "spec: {kubernetes: {distribution: k3s, apiEndpoint: \"api.lab:6443\"}, "+onBYOH+"}",
		`spec.kubernetes.apiEndpoint "api.lab:6443": kube-vip holds an IPv4 address, not a name`)
	expectErrors(t, "spec: {kubernetes: {distribution: k3s, apiEndpoint: \"192.168.1.100:8443\"}, "+onBYOH+"}",
		`spec.kubernetes.apiEndpoint "192.168.1.100:8443": with kube-vip the port is the API server's own, 6443`)
	mustParse(t, header+"spec: {kubernetes: {distribution: k3s, apiEndpoint: \"api.lab:8443\", apiLoadBalancer: external}, "+onBYOH+"}\n")
}

// Cilium's load balancer works through its kube-proxy replacement; kube-vip's BGP mode needs
// a router ID per node, which tow doesn't set yet.
func TestServiceLBLimits(t *testing.T) {
	pool := "addresses: [192.168.1.200-192.168.1.210]"
	expectErrors(t, "spec: {kubernetes: {distribution: k3s, networking: {cni: cilium}, serviceLoadBalancer: {provider: cilium, cilium: {"+pool+"}}}, "+onBYOH+"}",
		"provider cilium needs spec.kubernetes.networking.cilium.kubeProxyReplacement: true")
	mustParse(t, header+"spec: {kubernetes: {distribution: k3s, networking: {cni: cilium, cilium: {kubeProxyReplacement: true}}, serviceLoadBalancer: {provider: cilium, cilium: {"+pool+"}}}, "+onBYOH+"}\n")
	expectErrors(t, "spec: {kubernetes: {distribution: k3s, serviceLoadBalancer: {provider: kube-vip, kube-vip: {"+pool+", bgp: {localASN: 64512, peers: [{address: 192.168.1.1, asn: 64513}]}}}}, "+onBYOH+"}",
		"kube-vip.mode bgp isn't supported yet")
}

func TestExtraArgs(t *testing.T) {
	c := mustParse(t, header+`spec:
  flavor: local
  kubernetes:
    apiServer: {extraArgs: {audit-log-maxage: 30, anonymous-auth: false, service-account-issuer: [https://a.example, https://b.example]}}
    kubelet: {extraArgs: {max-pods: 150, v: 2}}
  workers: [{count: 1, kubelet: {extraArgs: {max-pods: 250}}}]
`)
	a := c.Spec.Kubernetes.APIServer.ExtraArgs
	if !slices.Equal(a["audit-log-maxage"].Values, []string{"30"}) || !slices.Equal(a["anonymous-auth"].Values, []string{"false"}) ||
		!slices.Equal(a["service-account-issuer"].Values, []string{"https://a.example", "https://b.example"}) {
		t.Errorf("args = %v", a)
	}
	f := c.Fields()
	if f["spec.kubernetes.apiServer.extraArgs.audit-log-maxage"] != "30" || f["spec.kubernetes.apiServer.extraArgs.service-account-issuer[1]"] != "https://b.example" {
		t.Errorf("effective config should read like the file: %v", f)
	}
	// A pool's kubelet flags win over the cluster's; the others are kept.
	w := c.KubeletArgs(&c.Spec.Workers[0].Group)
	if !slices.Equal(w["max-pods"].Values, []string{"250"}) || !slices.Equal(w["v"].Values, []string{"2"}) {
		t.Errorf("worker kubelet args = %v", w)
	}
	if cp := c.KubeletArgs(&c.Spec.ControlPlane.Group); !slices.Equal(cp["max-pods"].Values, []string{"150"}) {
		t.Errorf("control plane kubelet args = %v", cp)
	}

	expectErrors(t, `spec:
  kubernetes:
    distribution: k3s
    apiServer: {extraArgs: {"--v": 2, service-cluster-ip-range: 10.0.0.0/16, advertise-address: 1.2.3.4, audit-log-maxage: , feature-gates: []}}
    controllerManager: {extraArgs: {cluster_cidr: 10.0.0.0/16}}
    kubelet: {extraArgs: {node-labels: a=b, cluster-domain: x}}
    k3s:
      server: {extraArgs: {node-name: n1, kube-apiserver-arg: x=y, tls-san: a, disable: traefik}}
      agent: {extraArgs: {node-taint: "a=b:NoSchedule"}}
  `+onAWS+`
  workers: [{count: 1, kubelet: {extraArgs: {node-ip: 10.0.0.1}}}]`,
		`spec.kubernetes.apiServer.extraArgs: "--v" must be a flag name without leading dashes`,
		"spec.kubernetes.apiServer.extraArgs: service-cluster-ip-range conflicts with spec.kubernetes.networking.serviceCIDR",
		"spec.kubernetes.apiServer.extraArgs: advertise-address conflicts with a flag tow sets itself",
		"spec.kubernetes.apiServer.extraArgs: audit-log-maxage has no value (write true for an on/off flag)",
		"spec.kubernetes.apiServer.extraArgs: feature-gates has no value",
		"spec.kubernetes.controllerManager.extraArgs: cluster_cidr conflicts with spec.kubernetes.networking.podCIDR",
		"spec.kubernetes.kubelet.extraArgs: node-labels conflicts with the pools' labels",
		"spec.kubernetes.kubelet.extraArgs: cluster-domain conflicts with spec.kubernetes.networking.clusterDomain",
		"spec.kubernetes.k3s.server.extraArgs: node-name conflicts with a flag tow sets itself",
		"spec.kubernetes.k3s.server.extraArgs: kube-apiserver-arg conflicts with spec.kubernetes.apiServer.extraArgs",
		"spec.kubernetes.k3s.server.extraArgs: tls-san conflicts with spec.kubernetes.apiServer.certSANs",
		"spec.kubernetes.k3s.server.extraArgs: disable conflicts with a flag tow sets itself",
		"spec.kubernetes.k3s.agent.extraArgs: node-taint conflicts with the pools' taints",
		"spec.workers[0].kubelet.extraArgs: node-ip conflicts with a flag tow sets itself")
	expectErrors(t, "spec: {kubernetes: {distribution: k3s, apiServer: {extraArgs: {x: {a: b}}}}, "+onAWS+"}",
		"a flag takes a plain value, a list of them to repeat it, or {env: VARIABLE}")
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm, k3s: {server: {extraArgs: {secrets-encryption: true}}}}, "+onAWS+"}",
		"spec.kubernetes.k3s is set but distribution is kubeadm")
	expectErrors(t, "spec: {kubernetes: {distribution: k3s, apiServer: {extraArgs: {x: {env: X, other: y}}}}, "+onAWS+"}",
		"a flag takes a plain value, a list of them to repeat it, or {env: VARIABLE} for a secret")
	expectErrors(t, "spec: {flavor: local, kubernetes: {etcd: {extraArgs: {heartbeat-interval: 500}}}}",
		"spec.kubernetes.etcd.extraArgs isn't used by a single k3d server: it keeps its data in SQLite, not etcd")
	mustParse(t, header+"spec: {flavor: local, kubernetes: {etcd: {extraArgs: {heartbeat-interval: 500}}}, controlPlane: {count: 3}}\n")
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm, apiServer: {certSANs: [\"bad host!\"]}}, "+onAWS+"}",
		`spec.kubernetes.apiServer.certSANs[0] "bad host!" must be a DNS name or an IP address`)
}

func TestLabelsAndTaints(t *testing.T) {
	expectErrors(t, `spec:
  kubernetes: {distribution: kubeadm}
  `+onAWS+`
  controlPlane:
    labels: {kubernetes.io/hostname: x, node-role.kubernetes.io/control-plane: "", example.k8s.io/x: y, "Bad Key": x, ok: "bad value!", topology.kubernetes.io/zone: a}
    taints:
      - {key: dedicated, effect: NoSchedulee}
      - {key: a, value: b, effect: NoSchedule}
      - {key: a, effect: NoSchedule}
      - {key: node-role.kubernetes.io/control-plane, effect: NoSchedule}
      - {key: "x/y/z", value: "no way", effect: NoExecute}`,
		"spec.controlPlane.labels: kubernetes.io/hostname is set by the kubelet itself",
		"spec.controlPlane.labels: node-role.kubernetes.io/control-plane conflicts with a label the distribution sets itself",
		"spec.controlPlane.labels: example.k8s.io/x uses a reserved prefix (kubernetes.io, k8s.io)",
		"spec.controlPlane.labels: Bad Key must be at most 63 letters",
		`spec.controlPlane.labels: the value of ok, "bad value!", must be at most 63 letters`,
		`spec.controlPlane.taints[0].effect "NoSchedulee" must be NoSchedule, PreferNoSchedule or NoExecute`,
		"spec.controlPlane.taints[2]: a:NoSchedule is already set",
		"spec.controlPlane.taints[3]: node-role.kubernetes.io/control-plane conflicts with spec.controlPlane.schedulable",
		`spec.controlPlane.taints[4].key "x/y/z" must be at most 63 letters`,
		`spec.controlPlane.taints[4].value "no way" must be at most 63 letters`)
	if e := errorOf(header + "spec: {kubernetes: {distribution: kubeadm}, " + onAWS + ", controlPlane: {labels: {topology.kubernetes.io/zone: a, node.kubernetes.io/x: y, kubelet.kubernetes.io/z: w, node-role.kubernetes.io/infra: \"\"}}}\n"); strings.Contains(e, "labels") {
		t.Errorf("labels nodes may carry must be accepted: %s", e)
	}

	// Role labels go through the API; the kubelet registers the node with the others.
	kubelet, api := SplitLabels(map[string]string{"zone": "a", "node-role.kubernetes.io/worker": ""})
	if len(kubelet) != 1 || kubelet["zone"] != "a" || len(api) != 1 || api["node-role.kubernetes.io/worker"] != "" {
		t.Errorf("split: kubelet %v, api %v", kubelet, api)
	}
}

func TestSchedulable(t *testing.T) {
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm}, "+onAWS+", controlPlane: {schedulable: false}}",
		"spec.controlPlane.schedulable is false but there are no workers: your workloads would have nowhere to run")
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm}, "+onAWS+", controlPlane: {schedulable: maybe}}",
		`spec.controlPlane.schedulable "maybe" must be auto, true or false`)
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm}, "+onAWS+", workers: [{count: 1, schedulable: true}]}",
		"field schedulable not found")

	for _, tt := range []struct {
		spec string
		want bool
	}{
		{"controlPlane: {schedulable: auto}", true}, // no workers
		{"workers: [{count: 1}]", false},
		{"controlPlane: {schedulable: true}, workers: [{count: 1}]", true},
		{"controlPlane: {schedulable: false}, workers: [{count: 1}]", false},
	} {
		c := mustParse(t, header+"spec: {kubernetes: {distribution: kubeadm}, "+onAWS+", "+tt.spec+"}\n")
		if got := c.ControlPlaneSchedulable(); got != tt.want {
			t.Errorf("%s: schedulable = %v, want %v", tt.spec, got, tt.want)
		}
	}
}

func TestDefaultClassWarnings(t *testing.T) {
	c := mustParse(t, header+`spec:
  kubernetes:
    distribution: kubeadm
    storage: {csi: [local-path, openebs]}
    networking: {cni: cilium, cilium: {kubeProxyReplacement: true}}
    serviceLoadBalancer: {provider: cilium, cilium: {mode: bgp, addresses: [10.200.0.0/24], bgp: {localASN: 64512, peers: [{address: 10.0.0.1, asn: 64513}]}}}
    ingress: {controllers: [traefik, cilium]}
  `+onAWS+"\n")
	w := strings.Join(c.Warnings(), "\n")
	for _, want := range []string{
		"no default StorageClass: volume claims that name no class stay Pending\n  → set spec.kubernetes.storage.defaultClass",
		"no default IngressClass: Ingresses that name no class are served by no controller\n  → set spec.kubernetes.ingress.defaultClass",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("warnings missing %q:\n%s", want, w)
		}
	}
	// One controller is the default, with no warning.
	c = mustParse(t, header+"spec: {kubernetes: {distribution: kubeadm, networking: {cni: calico}, ingress: {controllers: [traefik]}}, "+onAWS+"}\n")
	if c.Spec.Kubernetes.Ingress.DefaultClass != IngressTraefik || strings.Contains(strings.Join(c.Warnings(), "\n"), "IngressClass") {
		t.Errorf("default class %q, warnings %v", c.Spec.Kubernetes.Ingress.DefaultClass, c.Warnings())
	}
}

func errorOf(yaml string) string {
	if _, err := Parse([]byte(yaml)); err != nil {
		return err.Error()
	}
	return ""
}

// A flag that takes a secret names the environment variable holding it; the value is never in
// the file, the effective config or the state.
func TestSecretFlagsFromEnv(t *testing.T) {
	c := mustParse(t, header+`spec:
  kubernetes:
    distribution: k3s
    k3s:
      server: {extraArgs: {token: {env: K3S_TOKEN}, etcd-s3-access-key: {env: S3_ACCESS_KEY}, etcd-s3-secret-key: {env: S3_SECRET_KEY}}}
    storage: {csi: [smb], smb: {source: //nas/k8s, usernameEnv: SMB_USER, passwordEnv: SMB_PASSWORD}}
    addons:
      helmCharts:
        - {name: db, repo: https://charts.example, chart: db, version: 1.0.0, values: {auth: {username: app}},
           valuesFromEnv: {auth.password: DB_PASSWORD, auth.replicationPassword: DB_PASSWORD}}
  `+onBYOHBlock+`
  workers: [{name: w, machines: [{host: 192.168.1.20}], kubelet: {extraArgs: {some-flag: {env: KUBELET_SECRET}}}}]
`)
	if v := c.Spec.Kubernetes.K3s.Server.ExtraArgs["token"]; v.Env != "K3S_TOKEN" || len(v.Values) != 0 {
		t.Errorf("token = %+v", v)
	}
	want := []string{"DB_PASSWORD", "K3S_TOKEN", "KUBELET_SECRET", "S3_ACCESS_KEY", "S3_SECRET_KEY", "SMB_PASSWORD", "SMB_USER"}
	if got := c.SecretEnv(); !slices.Equal(got, want) {
		t.Errorf("SecretEnv = %v, want %v", got, want)
	}
	if f := c.Fields(); f["spec.kubernetes.k3s.server.extraArgs.token.env"] != "K3S_TOKEN" {
		t.Errorf("the effective config must hold the variable's name: %v", f)
	}
	// k3s logs the flags it gives components (the kubelet here), not its own (etcd-s3-*, token).
	w := strings.Join(c.Warnings(), "\n")
	if !strings.Contains(w, "spec.workers[0].kubelet.extraArgs: some-flag comes from an environment variable, but k3s writes the flags it gives components into its log") {
		t.Errorf("k3s log warning missing: %q", w)
	}
	if strings.Contains(w, "etcd-s3") || strings.Contains(w, "token") {
		t.Errorf("k3s's own flags stay out of its log: no warning expected: %q", w)
	}

	// Local clusters take secrets too, on k3d's command line; k3d makes its own join token.
	c = mustParse(t, header+"spec: {flavor: local, kubernetes: {k3s: {server: {extraArgs: {etcd-s3-secret-key: {env: S3_SECRET_KEY}}}}}}\n")
	if w := strings.Join(c.Warnings(), "\n"); !strings.Contains(w, "etcd-s3-secret-key comes from an environment variable, but local clusters pass it on k3d's command line: 'docker inspect' on this machine shows it") {
		t.Errorf("local warning missing: %q", w)
	}
	expectErrors(t, "spec: {flavor: local, kubernetes: {k3s: {server: {extraArgs: {token: {env: K3S_TOKEN}}}}}}",
		"spec.kubernetes.k3s.server.extraArgs: token conflicts with a flag tow sets itself")

	// Written in the file, a secret flag is refused without showing its value.
	_, err := Parse([]byte(header + "spec: {kubernetes: {distribution: k3s, k3s: {server: {extraArgs: {etcd-s3-secret-key: hunter22}}}}, " + onAWS + "}\n"))
	if err == nil || !strings.Contains(err.Error(), "spec.kubernetes.k3s.server.extraArgs.etcd-s3-secret-key") ||
		!strings.Contains(err.Error(), "{env: VARIABLE} in extraArgs") || strings.Contains(err.Error(), "hunter22") {
		t.Errorf("expected a refusal naming the flag and the {env} form, without the value: %v", err)
	}

	expectErrors(t, "spec: {kubernetes: {distribution: k3s, k3s: {agent: {extraArgs: {token: {env: K3S_TOKEN}}}}}, "+onAWS+"}",
		"spec.kubernetes.k3s.agent.extraArgs: token conflicts with a flag tow sets itself")
	expectErrors(t, "spec: {kubernetes: {distribution: k3s, k3s: {server: {extraArgs: {token-file: {env: X}}}}}, "+onAWS+"}",
		"spec.kubernetes.k3s.server.extraArgs: token-file conflicts with a flag tow sets itself")
	expectErrors(t, "spec: {kubernetes: {distribution: k3s, apiServer: {extraArgs: {x: {env: 1BAD}}}}, "+onAWS+"}",
		`spec.kubernetes.apiServer.extraArgs: x: "1BAD" must name an environment variable`)
	expectErrors(t, `spec:
  kubernetes:
    distribution: kubeadm
    addons:
      helmCharts:
        - {name: db, repo: https://charts.example, chart: db, version: 1.0.0, values: {auth: {password: ""}},
           valuesFromEnv: {auth.password: DB_PASSWORD, "bad path": X, ok: lower-case-is-not-a-var}}
  `+onAWS,
		"spec.kubernetes.addons.helmCharts[0].valuesFromEnv: auth.password is also set in values",
		`spec.kubernetes.addons.helmCharts[0].valuesFromEnv: "bad path" must be a value's path`,
		`spec.kubernetes.addons.helmCharts[0].valuesFromEnv: ok: "lower-case-is-not-a-var" must name an environment variable`)

	// kubeadm puts component flags on a command line: the value can be seen on the node.
	c = mustParse(t, header+"spec: {kubernetes: {distribution: kubeadm, apiServer: {extraArgs: {some-flag: {env: X}}}}, "+onAWS+"}\n")
	if w := strings.Join(c.Warnings(), "\n"); !strings.Contains(w, "spec.kubernetes.apiServer.extraArgs: some-flag comes from an environment variable, but kubeadm passes it on the component's command line") {
		t.Errorf("exposure warning missing: %q", w)
	}
}
