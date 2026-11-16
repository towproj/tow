package config

import (
	"path/filepath"
	"strings"
	"testing"
)

const header = "apiVersion: towproj.github.io/v1alpha1\nkind: Cluster\nmetadata: {name: x}\n"

func mustParse(t *testing.T, yaml string) *Config {
	t.Helper()
	c, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("%v\nfor:\n%s", err, yaml)
	}
	return c
}

// Every sample in examples/ must stay valid; integration tests build clusters from them.
func TestExamplesParse(t *testing.T) {
	files, err := filepath.Glob("../../examples/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples found: %v", err)
	}

	want := map[string]struct{ distribution, mode string }{
		"local.yaml":                  {DistributionK3d, ModeLocal},
		"homelab.yaml":                {DistributionK3s, ModeBYOH},
		"homelab-proxmox.yaml":        {DistributionK3s, ModeProvision},
		"homelab-proxmox-ha.yaml":     {DistributionKubeadm, ModeProvision},
		"homelab-proxmox-rocky.yaml":  {DistributionK3s, ModeProvision},
		"homelab-aws-almalinux.yaml":  {DistributionK3s, ModeProvision},
		"dev-test-proxmox-rocky.yaml": {DistributionKubeadm, ModeProvision},
		"dev-test-aws-almalinux.yaml": {DistributionKubeadm, ModeProvision},
		"storage-proxmox.yaml":        {DistributionKubeadm, ModeProvision},
		"storage-aws.yaml":            {DistributionKubeadm, ModeProvision},
		"metallb-proxmox.yaml":        {DistributionKubeadm, ModeProvision},
		"kube-vip-lb-proxmox.yaml":    {DistributionK3s, ModeProvision},
		"cilium-lb-proxmox.yaml":      {DistributionK3s, ModeProvision},
		"ingress-proxmox.yaml":        {DistributionKubeadm, ModeProvision},
		"cilium-ingress-proxmox.yaml": {DistributionK3s, ModeProvision},
		"proxmox-csi.yaml":            {DistributionK3s, ModeProvision},
		"ebs-aws.yaml":                {DistributionKubeadm, ModeProvision},
		"cnpg-aws.yaml":               {DistributionKubeadm, ModeProvision},
		"local-helm.yaml":             {DistributionK3d, ModeLocal},
		"calico-proxmox.yaml":         {DistributionK3s, ModeProvision},
		"calico-ebpf-proxmox.yaml":    {DistributionK3s, ModeProvision},
		"cilium-proxmox.yaml":         {DistributionK3s, ModeProvision},
		"dev-test.yaml":               {DistributionKubeadm, ModeBYOH},
		"dev-test-aws.yaml":           {DistributionKubeadm, ModeProvision},
		"dev-test-proxmox.yaml":       {DistributionKubeadm, ModeProvision},
		"production.yaml":             {DistributionKubeadm, ModeBYOH},
		"production-aws.yaml":         {DistributionKubeadm, ModeProvision},
		"production-proxmox.yaml":     {DistributionKubeadm, ModeProvision},
		"custom-homelab-ha.yaml":      {DistributionK3s, ModeBYOH},
		"custom-proxmox.yaml":         {DistributionKubeadm, ModeProvision},
		"custom-bare.yaml":            {DistributionK3d, ModeLocal},
		"custom-full.yaml":            {DistributionKubeadm, ModeBYOH},
		"dev-test-gcp.yaml":           {DistributionKubeadm, ModeProvision},
		"dev-test-gcp-almalinux.yaml": {DistributionKubeadm, ModeProvision},
		"pd-gcp.yaml":                 {DistributionKubeadm, ModeProvision},
		"homelab-gcp-ha.yaml":         {DistributionK3s, ModeProvision},
		"dev-test-azure.yaml":         {DistributionKubeadm, ModeProvision},
		"disk-azure.yaml":             {DistributionKubeadm, ModeProvision},
		"homelab-azure-ha.yaml":       {DistributionK3s, ModeProvision},
	}

	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			c, err := Load(f)
			if err != nil {
				t.Fatalf("example invalid:\n%v", err)
			}
			w, ok := want[filepath.Base(f)]
			if !ok {
				t.Fatalf("new example %s: add its expected distribution and mode to this test", filepath.Base(f))
			}
			if c.Distribution() != w.distribution || c.Mode != w.mode {
				t.Errorf("distribution/mode = %s/%s, want %s/%s", c.Distribution(), c.Mode, w.distribution, w.mode)
			}
		})
	}
}

// What the file says wins, then the flavor, then the distribution's default.
func TestDefaultsOrder(t *testing.T) {
	// Flavor only.
	c := mustParse(t, header+"spec: {flavor: production, infrastructure: {provider: aws, aws: {region: eu-west-3}}}\n")
	k := c.Spec.Kubernetes
	if k.Distribution != DistributionKubeadm || k.Networking.CNI != CNICilium || k.Version != "" || k.Release() != DefaultKubernetesVersion || k.ContainerRuntime != "containerd" {
		t.Errorf("flavor not applied: %+v", k)
	}
	if c.ControlPlaneCount() != 3 || c.WorkerCount() != 2 || c.Spec.Workers[0].Name != DefaultPoolName {
		t.Errorf("flavor counts: %d control planes, %d workers, pool %q", c.ControlPlaneCount(), c.WorkerCount(), c.Spec.Workers[0].Name)
	}
	if k.Networking.PodCIDR != "10.244.0.0/16" || k.Networking.ServiceCIDR != "10.96.0.0/12" {
		t.Errorf("distribution ranges not applied: %+v", k.Networking)
	}
	if c.Spec.Infrastructure.Access.SSH.User != DefaultSSHUser {
		t.Errorf("provisioned machines default to the cloud image's user, got %q", c.Spec.Infrastructure.Access.SSH.User)
	}

	// The file overrides every flavor value.
	c = mustParse(t, header+`spec:
  flavor: production
  kubernetes: {distribution: k3s, networking: {cni: flannel}}
  infrastructure: {provider: aws, aws: {region: eu-west-3}}
  controlPlane: {count: 1}
  workers: []
`)
	if c.Distribution() != DistributionK3s || c.Spec.Kubernetes.Networking.CNI != CNIFlannel || c.ControlPlaneCount() != 1 || c.WorkerCount() != 0 {
		t.Errorf("overrides lost: %s %s %d %d", c.Distribution(), c.Spec.Kubernetes.Networking.CNI, c.ControlPlaneCount(), c.WorkerCount())
	}
	if c.Spec.Kubernetes.Networking.PodCIDR != "10.42.0.0/16" {
		t.Errorf("k3s ranges expected, got %s", c.Spec.Kubernetes.Networking.PodCIDR)
	}

	// No flavor at all: the distribution's ranges, one control plane, and no CNI.
	c = mustParse(t, header+"spec: {kubernetes: {distribution: kubeadm}, infrastructure: {provider: aws, aws: {region: eu-west-3}}}\n")
	if c.Spec.Kubernetes.Networking.CNI != CNINone || c.ControlPlaneCount() != 1 || c.WorkerCount() != 0 {
		t.Errorf("no-flavor defaults: cni %s, %d control planes, %d workers", c.Spec.Kubernetes.Networking.CNI, c.ControlPlaneCount(), c.WorkerCount())
	}
}

// The architecture is whatever the file says, as long as it technically works.
func TestCustomArchitectures(t *testing.T) {
	// A homelab with 2 control planes and Cilium on k3s: accepted, with the etcd warning.
	c, err := Load("../../examples/custom-homelab-ha.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.ControlPlaneCount() != 2 || c.Spec.Kubernetes.Networking.CNI != CNICilium {
		t.Errorf("custom homelab: %d control planes, cni %s", c.ControlPlaneCount(), c.Spec.Kubernetes.Networking.CNI)
	}
	if w := strings.Join(c.Warnings(), "\n"); !strings.Contains(w, "2 control planes tolerate no more failures than 1") {
		t.Errorf("warnings = %q", w)
	}
	if m := c.Spec.Workers[0].Machines[1]; m.SSH.Port != 2222 || m.SSH.User != "pi" || m.SSH.KeyPath != "~/.ssh/id_ed25519" {
		t.Errorf("per-machine ssh merged wrong: %+v", m.SSH)
	}

	// kubeadm with flannel, several worker pools of different sizes.
	c, err = Load("../../examples/custom-proxmox.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.Spec.Kubernetes.Networking.CNI != CNIFlannel || c.MachineCount() != 3 || c.Spec.Workers[1].Resources.CPU != 3 || c.Spec.Workers[0].Disk != DefaultProxmoxDisk {
		t.Errorf("custom proxmox: cni %s, %d machines, pools %+v", c.Spec.Kubernetes.Networking.CNI, c.MachineCount(), c.Spec.Workers)
	}

	// A local cluster with 3 servers and 2 agents.
	c = mustParse(t, header+"spec: {flavor: local, controlPlane: {count: 3}, workers: [{count: 2}]}\n")
	if c.ControlPlaneCount() != 3 || c.WorkerCount() != 2 || c.Mode != ModeLocal {
		t.Errorf("local topology: %d/%d %s", c.ControlPlaneCount(), c.WorkerCount(), c.Mode)
	}
	if len(c.Warnings()) != 0 {
		t.Errorf("3 servers and agents deserve no warning: %v", c.Warnings())
	}
}

func TestMinimalFiles(t *testing.T) {
	for _, spec := range []string{
		"spec: {flavor: local}",
		"spec: {infrastructure: {provider: local}}",
	} {
		c := mustParse(t, header+spec+"\n")
		if c.Distribution() != DistributionK3d || c.ControlPlaneCount() != 1 || c.WorkerCount() != 0 {
			t.Errorf("%s: %s %d/%d", spec, c.Distribution(), c.ControlPlaneCount(), c.WorkerCount())
		}
	}
}

func TestParseErrors(t *testing.T) {
	const aws = "infrastructure: {provider: aws, aws: {region: eu-west-3}}"
	tests := []struct {
		name string
		yaml string   // the spec block, or a full file when it starts with "apiVersion"
		want []string // substrings that must all appear in the error
	}{
		{"wrong apiVersion", "apiVersion: v1\nkind: Cluster\n", []string{`apiVersion "v1" is not supported`}},
		{"wrong kind", "apiVersion: towproj.github.io/v1alpha1\nkind: Pod\n", []string{`kind "Pod" is not supported`}},
		{"unknown field", "spec: {flavor: local, contolPlane: {count: 1}}", []string{"contolPlane"}},
		{"no provider", "spec: {kubernetes: {distribution: k3s}}", []string{"spec.infrastructure.provider is required", "aws, gcp, azure, proxmox, byoh or local"}},
		{"unknown flavor", "spec: {flavor: tiny}", []string{`spec.flavor "tiny" is unknown`, "a flavor is optional"}},
		{"no distribution without flavor", "spec: {" + aws + "}", []string{"spec.kubernetes.distribution is required without a flavor"}},
		{"k3d on machines", "spec: {kubernetes: {distribution: k3d}, " + aws + "}", []string{"k3d runs only on provider local"}},
		{"unknown cni", "spec: {kubernetes: {distribution: kubeadm, networking: {cni: weave}}, " + aws + "}", []string{`spec.kubernetes.networking.cni: "weave" is unknown (calico, cilium, flannel or none)`}},
		{"cni moved", "spec: {kubernetes: {distribution: kubeadm, cni: calico}, " + aws + "}", []string{"field cni not found"}},
		{"k3s release on kubeadm", "spec: {kubernetes: {distribution: kubeadm, version: v1.33.6+k3s1}, " + aws + "}", []string{"is a k3s release"}},
		{"bad name", "apiVersion: towproj.github.io/v1alpha1\nkind: Cluster\nmetadata: {name: Bad_Name}\nspec: {flavor: local}\n", []string{"DNS label"}},
		{"zero control planes", "spec: {flavor: local, controlPlane: {count: -1}}", []string{"spec.controlPlane.count must be at least 1"}},
		{"pool needs a count", "spec: {kubernetes: {distribution: kubeadm}, " + aws + ", workers: [{name: a}]}", []string{"spec.workers[0].count must be at least 1"}},
		{"pool names", "spec: {kubernetes: {distribution: kubeadm}, " + aws + ", workers: [{name: a, count: 1}, {name: a, count: 1}, {count: 1}, {name: cp, count: 1}]}",
			[]string{`"a" is used twice`, "spec.workers[2].name is required", `"cp" must be a lowercase DNS label other than "cp"`}},
		{"machine names too long", "apiVersion: towproj.github.io/v1alpha1\nkind: Cluster\nmetadata: {name: " + strings.Repeat("a", 50) + "}\nspec: {kubernetes: {distribution: kubeadm}, " + aws + ", workers: [{name: generalpurpose, count: 1}]}\n",
			[]string{"hostnames allow 63"}},
		{"HA byoh needs an endpoint", `spec:
  kubernetes: {distribution: kubeadm}
  infrastructure: {provider: byoh, access: {ssh: {user: u}}}
  controlPlane: {machines: [{host: 10.0.0.1}, {host: 10.0.0.2}, {host: 10.0.0.3}]}`, []string{"spec.kubernetes.apiEndpoint is required with 3 control planes"}},
		{"byoh machines", `spec:
  kubernetes: {distribution: k3s}
  infrastructure: {provider: byoh}
  controlPlane: {machines: [{host: 10.0.0.1}, {host: ""}]}
  workers: [{machines: [{host: 10.0.0.1, ssh: {user: u, port: 70000}}]}]`,
			[]string{"an SSH user is required", "spec.controlPlane.machines[1].host is required", `host "10.0.0.1" is already listed`, "ssh port 70000 out of range"}},
		{"byoh without control plane", "spec: {kubernetes: {distribution: k3s}, infrastructure: {provider: byoh, access: {ssh: {user: u}}}}", []string{"spec.controlPlane.machines needs at least one machine"}},
		{"ranges overlap each other", "spec: {kubernetes: {distribution: kubeadm, networking: {podCIDR: 10.0.0.0/8, serviceCIDR: 10.96.0.0/12}}, " + aws + "}",
			[]string{"podCIDR 10.0.0.0/8 and serviceCIDR 10.96.0.0/12 overlap"}},
		{"ranges overlap the VPC", "spec: {kubernetes: {distribution: kubeadm, networking: {podCIDR: 10.250.0.0/20}}, " + aws + "}",
			[]string{"podCIDR 10.250.0.0/20 overlaps the AWS VPC (10.250.0.0/16)"}},
		{"ranges overlap byoh machines", `spec:
  kubernetes: {distribution: k3s}
  infrastructure: {provider: byoh, access: {ssh: {user: u}}}
  controlPlane: {machines: [{host: 10.42.0.5}]}`, []string{"podCIDR 10.42.0.0/16 overlaps machine 10.42.0.5"}},
		{"bad ranges", "spec: {kubernetes: {distribution: kubeadm, networking: {podCIDR: lots}}, " + aws + "}", []string{`podCIDR "lots" must be an IPv4 range`}},
		{"aws needs a region", "spec: {kubernetes: {distribution: kubeadm}, infrastructure: {provider: aws}}", []string{"spec.infrastructure.aws.region is required"}},
		{"aws disk too small", "spec: {kubernetes: {distribution: kubeadm}, " + aws + ", controlPlane: {disk: 8Gi}}", []string{"spec.controlPlane.disk is 8Gi; a kubeadm control plane needs at least 14Gi"}},
		{"aws bad disk", "spec: {kubernetes: {distribution: kubeadm}, " + aws + ", controlPlane: {disk: \"40\"}}", []string{`"40" is not a size`}},
		{"admin ranges", "spec: {kubernetes: {distribution: kubeadm}, infrastructure: {provider: aws, aws: {region: x}, access: {adminCIDRs: [nope]}}}", []string{`adminCIDRs[0] "nope"`}},
		{"gcp needs a project and region", "spec: {kubernetes: {distribution: kubeadm}, infrastructure: {provider: gcp}}",
			[]string{"spec.infrastructure.gcp.project is required", "spec.infrastructure.gcp.region is required, like europe-west1"}},
		{"gcp bad project and zone", "spec: {kubernetes: {distribution: kubeadm}, infrastructure: {provider: gcp, gcp: {project: My_Project, region: europe-west1-b}}}",
			[]string{`spec.infrastructure.gcp.project "My_Project" isn't a project ID`, `spec.infrastructure.gcp.region "europe-west1-b" isn't a region`}},
		{"gcp name starts with a letter", "apiVersion: towproj.github.io/v1alpha1\nkind: Cluster\nmetadata: {name: 1lab}\nspec: {kubernetes: {distribution: k3s}, infrastructure: {provider: gcp, gcp: {project: my-project, region: europe-west1}}}\n",
			[]string{`metadata.name "1lab": on gcp a cluster's name starts with a letter and has at most 50 characters`}},
		{"gcp ranges overlap the subnet", "spec: {kubernetes: {distribution: kubeadm, networking: {podCIDR: 10.250.0.0/20}}, infrastructure: {provider: gcp, gcp: {project: my-project, region: europe-west1}}}",
			[]string{"podCIDR 10.250.0.0/20 overlaps the GCP VPC (10.250.0.0/16)"}},
		{"azure needs a subscription and location", "spec: {kubernetes: {distribution: kubeadm}, infrastructure: {provider: azure}}",
			[]string{"spec.infrastructure.azure.subscription is required", "spec.infrastructure.azure.location is required, like westeurope"}},
		{"azure bad subscription and location", "spec: {kubernetes: {distribution: kubeadm}, infrastructure: {provider: azure, azure: {subscription: my-sub, location: West Europe}}}",
			[]string{`spec.infrastructure.azure.subscription "my-sub" isn't a subscription ID`, `spec.infrastructure.azure.location "West Europe" isn't a location's name`}},
		{"azure ranges overlap the VNet", "spec: {kubernetes: {distribution: kubeadm, networking: {podCIDR: 10.250.0.0/20}}, infrastructure: {provider: azure, azure: {subscription: 00000000-0000-0000-0000-000000000000, location: westeurope}}}",
			[]string{"podCIDR 10.250.0.0/20 overlaps the Azure VNet (10.250.0.0/16)"}},
		{"gcp disk too small", "spec: {kubernetes: {distribution: kubeadm}, infrastructure: {provider: gcp, gcp: {project: my-project, region: europe-west1}}, controlPlane: {disk: 8Gi}}",
			[]string{"spec.controlPlane.disk is 8Gi; a kubeadm control plane needs at least 14Gi"}},
		{"credentials", "spec: {flavor: local, infrastructure: {apiToken: x}}", []string{"looks like a credential"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			y := tt.yaml
			if !strings.HasPrefix(y, "apiVersion") {
				y = header + y + "\n"
			}
			_, err := Parse([]byte(y))
			if err == nil {
				t.Fatalf("expected an error for:\n%s", y)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error missing %q:\n%v", w, err)
				}
			}
		})
	}
}

// A field the provider doesn't use is refused as written, never silently ignored.
func TestFieldsRefusedPerProvider(t *testing.T) {
	tests := []struct {
		name string
		spec string
		want []string
	}{
		{"local", `spec:
  flavor: local
  kubernetes: {distribution: kubeadm, apiEndpoint: "1.2.3.4:6443", apiLoadBalancer: kube-vip, apiServer: {certSANs: [a.lan]}, networking: {podCIDR: 10.1.0.0/16, clusterDomain: lab.local}}
  infrastructure: {provider: local, network: {subnet: 10.0.0.0/24}, access: {ssh: {user: u}}, aws: {region: x}}
  controlPlane: {instanceType: t3.small, resources: {cpu: 2}, disk: 10Gi, machines: [{host: h}]}
  workers: [{count: 1}, {count: 1}]`, []string{
			`distribution "kubeadm" can't run locally`, "spec.kubernetes.apiLoadBalancer isn't used by local clusters",
			"spec.kubernetes.apiServer.certSANs isn't used by local clusters", "spec.kubernetes.networking.clusterDomain isn't used",
			"spec.kubernetes.apiEndpoint isn't used", "spec.kubernetes.networking.podCIDR isn't used", "spec.infrastructure.network isn't used",
			"spec.infrastructure.access isn't used", "spec.infrastructure.aws is for provider aws", "spec.workers can hold one pool",
			"spec.controlPlane.instanceType isn't used", "spec.controlPlane.resources isn't used", "spec.controlPlane.disk isn't used",
			"spec.controlPlane.machines isn't used"}},
		{"byoh", `spec:
  kubernetes: {distribution: k3s}
  infrastructure: {provider: byoh, network: {subnet: 10.0.0.0/24}, access: {ssh: {user: u}, adminCIDRs: [1.2.3.4/32]}, proxmox: {node: pve}}
  controlPlane: {count: 1, instanceType: t3.small, machines: [{host: h}]}`, []string{
			"spec.infrastructure.network isn't used with byoh", "spec.infrastructure.proxmox is for provider proxmox",
			"spec.controlPlane.count isn't used with byoh", "spec.controlPlane.instanceType isn't used with byoh"}},
		{"aws", `spec:
  kubernetes: {distribution: kubeadm, apiEndpoint: "1.2.3.4:6443", apiLoadBalancer: kube-vip}
  infrastructure: {provider: aws, aws: {region: x}, network: {subnet: 10.250.0.0/16, gateway: 10.250.0.1}, access: {ssh: {port: 2222}}}
  controlPlane: {resources: {cpu: 2}, machines: [{host: h}]}`, []string{
			"spec.infrastructure.network takes only subnet on aws", "spec.infrastructure.access.ssh.port isn't used for machines tow creates",
			"spec.kubernetes.apiEndpoint isn't used on aws", "spec.kubernetes.apiLoadBalancer isn't used on aws", "spec.controlPlane.resources isn't supported on aws yet; use instanceType",
			"spec.controlPlane.machines isn't used on aws"}},
		{"gcp", `spec:
  kubernetes: {distribution: kubeadm, apiEndpoint: "1.2.3.4:6443", apiLoadBalancer: kube-vip}
  infrastructure: {provider: gcp, gcp: {project: my-project, region: europe-west1}, aws: {region: x}, network: {subnet: 10.250.0.0/16, gateway: 10.250.0.1}, access: {ssh: {port: 2222}}}
  controlPlane: {resources: {cpu: 2}, machines: [{host: h}]}`, []string{
			"spec.infrastructure.network takes only subnet on gcp (the VPC's range); GCP assigns", "spec.infrastructure.access.ssh.port isn't used for machines tow creates",
			"spec.kubernetes.apiEndpoint isn't used on gcp", "spec.kubernetes.apiLoadBalancer isn't used on gcp", "spec.controlPlane.resources isn't supported on gcp yet; use instanceType (e.g. e2-standard-4)",
			"spec.controlPlane.machines isn't used on gcp", "spec.infrastructure.aws is for provider aws"}},
		{"azure", `spec:
  kubernetes: {distribution: kubeadm, apiEndpoint: "1.2.3.4:6443"}
  infrastructure: {provider: azure, azure: {subscription: 00000000-0000-0000-0000-000000000000, location: westeurope}, gcp: {project: p}, network: {subnet: 10.250.0.0/16, dns: [1.1.1.1]}}
  controlPlane: {resources: {cpu: 2}}`, []string{
			"spec.infrastructure.network takes only subnet on azure (the VNet's range); Azure assigns", "spec.kubernetes.apiEndpoint isn't used on azure",
			"spec.controlPlane.resources isn't supported on azure yet; use instanceType (e.g. Standard_B2ms)", "spec.infrastructure.gcp is for provider gcp"}},
		{"gcp block elsewhere", `spec:
  kubernetes: {distribution: kubeadm}
  infrastructure: {provider: aws, aws: {region: x}, gcp: {project: my-project}}`, []string{"spec.infrastructure.gcp is for provider gcp"}},
		{"proxmox", `spec:
  kubernetes: {distribution: kubeadm}
  infrastructure: {provider: proxmox, access: {ssh: {port: 2222}}}
  controlPlane: {instanceType: t3.small}`, []string{
			"spec.infrastructure.access.ssh.port isn't used", "spec.controlPlane.instanceType isn't used on proxmox: Proxmox has no instance types"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(header + tt.spec + "\n"))
			if err == nil {
				t.Fatal("expected refusals")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error missing %q:\n%v", w, err)
				}
			}
		})
	}
}

func TestWarnings(t *testing.T) {
	c := mustParse(t, header+"spec: {kubernetes: {distribution: kubeadm}, infrastructure: {provider: aws, aws: {region: x}}, controlPlane: {count: 4}}\n")
	w := strings.Join(c.Warnings(), "\n")
	if !strings.Contains(w, "4 control planes tolerate no more failures than 3") || !strings.Contains(w, "no workers") {
		t.Errorf("warnings = %q", w)
	}
	if c := mustParse(t, header+"spec: {flavor: production, infrastructure: {provider: aws, aws: {region: x}}}\n"); len(c.Warnings()) != 0 {
		t.Errorf("3 control planes and workers deserve no warning: %v", c.Warnings())
	}
}

func TestQuantities(t *testing.T) {
	for in, want := range map[string]int{"512Mi": 512, "2Gi": 2048, "1.5Gi": 1536, "1G": 954, "1Ti": 1 << 20} {
		if got, err := MiB(in); err != nil || got != want {
			t.Errorf("MiB(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if got, _ := GiB("20G"); got != 19 {
		t.Errorf("GiB(20G) = %d, want 19 (rounded up from 18.6)", got)
	}
	for _, bad := range []string{"4", "4GB", "-1Gi", "Gi", "lots"} {
		if _, err := MiB(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}
