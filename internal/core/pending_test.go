package core

import (
	"strings"
	"testing"
)

// Whatever this version can't install yet is named, never silently skipped.
func TestPending(t *testing.T) {
	if got := Pending(parse(t, "x", "{flavor: local}")); len(got) != 0 {
		t.Errorf("k3d installs everything the local flavor asks for: %v", got)
	}
	if got := Pending(load(t, "homelab.yaml")); len(got) != 0 {
		t.Errorf("k3s installs everything the homelab flavor asks for: %v", got)
	}

	if got := Pending(parse(t, "x", `
  flavor: local
  kubernetes:
    addons:
      helmCharts: [{name: cert-manager, repo: https://charts.jetstack.io, chart: cert-manager, version: v1.19.1}]
      manifests: [https://example.com/app.yaml]
  controlPlane: {labels: {node-role.kubernetes.io/infra: "", zone: a}}`)); len(got) != 0 {
		t.Errorf("charts and manifests are installed: %v", got)
	}

	// Storage of the second batch, service load balancers and ingress controllers wait.
	got := strings.Join(Pending(parse(t, "x", `
  kubernetes: {distribution: kubeadm, storage: {csi: [longhorn, rook-ceph], defaultClass: longhorn}, serviceLoadBalancer: {provider: metallb, metallb: {addresses: [192.168.1.200-192.168.1.210]}}, ingress: {controllers: [traefik]}}
  infrastructure: {provider: byoh, access: {ssh: {user: pi}}}
  controlPlane: {machines: [{host: 192.168.1.10}]}`)), "; ")
	if want := "storage rook-ceph"; got != want {
		t.Errorf("pending =\n%s\nwant\n%s", got, want)
	}

	if got := Pending(load(t, "custom-homelab-ha.yaml")); len(got) != 0 {
		t.Errorf("k3s on machines installs Cilium and kube-vip: %v", got)
	}

	// kubeadm installs Kubernetes and the CNI, but bundles none of k3s's add-ons.
	if got := Pending(parse(t, "x", "{kubernetes: {distribution: kubeadm, networking: {cni: flannel}}, infrastructure: {provider: aws, aws: {region: eu-west-3}}}")); len(got) != 0 {
		t.Errorf("kubeadm with flannel and nothing else: %v", got)
	}
	if got := Pending(load(t, "dev-test-aws.yaml")); len(got) != 0 {
		t.Errorf("dev-test on kubeadm installs everything: %v", got)
	}
	// AWS's own load balancer arrives later (the misc phase).
	if got := strings.Join(Pending(parse(t, "x", "{kubernetes: {distribution: kubeadm, serviceLoadBalancer: {provider: aws}}, infrastructure: {provider: aws, aws: {region: eu-west-3}}}")), "; "); got != "service load balancer aws" {
		t.Errorf("aws load balancer: %q", got)
	}
}

// A local cluster's settings reach k3d as k3s flags; secrets reach no one, only their names.
func TestLocalSpecFromConfig(t *testing.T) {
	cfg := parse(t, "x", `
  infrastructure: {provider: local}
  kubernetes:
    kubeconfig: {context: lab, merge: false}
    addons: {helmCharts: [{name: db, repo: https://charts.example, chart: db, version: 1.0.0, valuesFromEnv: {auth.password: DB_PASSWORD}}]}
  workers: [{count: 1, labels: {gpu: "true", node-role.kubernetes.io/gpu: ""}}]`)
	spec := specFromConfig(cfg, "v1.35.5+k3s1")
	k := spec.Kubernetes
	if k == nil || k.Context != "lab" || k.Merge || len(k.ServerFlags) == 0 || len(k.AgentFlags) != 1 || k.AgentFlags[0].Value != "gpu=true" {
		t.Errorf("kubernetes spec = %+v", k)
	}
	if _, ok := k.AgentRoleLabels["node-role.kubernetes.io/gpu"]; !ok || len(k.ServerRoleLabels) != 0 {
		t.Errorf("role labels: servers %v, agents %v", k.ServerRoleLabels, k.AgentRoleLabels)
	}
	if len(spec.SecretEnv) != 1 || spec.SecretEnv[0] != "DB_PASSWORD" {
		t.Errorf("secret variable names = %v", spec.SecretEnv)
	}
}
