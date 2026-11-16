package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

var (
	dnsLabel   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	k8sVersion = regexp.MustCompile(`^v1\.\d+\.\d+(\+k3s\d+)?$`)
)

// checkFieldsForProvider refuses fields the provider doesn't use, as the file wrote them: a
// misplaced field must never look like it worked.
func (c *Config) checkFieldsForProvider() []error {
	var errs []error
	refuse := func(path, why string) { errs = append(errs, fmt.Errorf("%s %s", path, why)) }
	s := c.Spec
	k := s.Kubernetes
	in := s.Infrastructure
	p := in.Provider

	// Groups: which sizing fields each provider takes.
	checkGroup := func(path string, g Group, allow map[string]string) {
		set := map[string]bool{
			"count": g.Count != 0, "instanceType": g.InstanceType != "", "resources": g.Resources != nil,
			"disk": g.Disk != "", "machines": len(g.Machines) > 0,
		}
		for _, f := range []string{"count", "instanceType", "resources", "disk", "machines"} {
			if why, refused := allow[f]; set[f] && refused {
				refuse(path+"."+f, why)
			}
		}
	}
	eachGroup := func(allow map[string]string) {
		checkGroup("spec.controlPlane", s.ControlPlane.Group, allow)
		for i, w := range s.Workers {
			checkGroup(fmt.Sprintf("spec.workers[%d]", i), w.Group, allow)
		}
	}

	if p != ProviderAWS && in.AWS != nil {
		refuse("spec.infrastructure.aws", "is for provider aws")
	}
	if p != ProviderGCP && in.GCP != nil {
		refuse("spec.infrastructure.gcp", "is for provider gcp")
	}
	if p != ProviderAzure && in.Azure != nil {
		refuse("spec.infrastructure.azure", "is for provider azure")
	}
	if p != ProviderProxmox && in.Proxmox != nil {
		refuse("spec.infrastructure.proxmox", "is for provider proxmox")
	}

	switch p {
	case ProviderLocal:
		const why = "isn't used by local clusters (k3d runs in Docker on this machine)"
		if k.Distribution != "" && k.Distribution != DistributionK3d {
			refuse("spec.kubernetes.distribution", fmt.Sprintf("%q can't run locally; local clusters are k3d", k.Distribution))
		}
		if k.APIEndpoint != "" {
			refuse("spec.kubernetes.apiEndpoint", "isn't used by local clusters (k3d puts its own load balancer in front of the servers)")
		}
		if k.APILoadBalancer != "" {
			refuse("spec.kubernetes.apiLoadBalancer", "isn't used by local clusters (k3d puts its own load balancer in front of the servers)")
		}
		if len(k.APIServer.CertSANs) > 0 {
			refuse("spec.kubernetes.apiServer.certSANs", "isn't used by local clusters (k3d sets the API certificate's names itself)")
		}
		n := k.Networking
		for _, f := range []struct{ name, v string }{{"podCIDR", n.PodCIDR}, {"serviceCIDR", n.ServiceCIDR}, {"clusterDomain", n.ClusterDomain}} {
			if f.v != "" {
				refuse("spec.kubernetes.networking."+f.name, "isn't used by local clusters (k3d brings its own)")
			}
		}
		if in.Network != nil {
			refuse("spec.infrastructure.network", why)
		}
		if in.Access.SSH != (SSH{}) || len(in.Access.AdminCIDRs) > 0 || len(in.Access.Ports) > 0 {
			refuse("spec.infrastructure.access", why)
		}
		if len(s.Workers) > 1 {
			refuse("spec.workers", "can hold one pool on local clusters: its count is k3d's number of agents")
		}
		eachGroup(map[string]string{
			"instanceType": why, "resources": why + "; containers share this machine's CPU and memory", "disk": why,
			"machines": why,
		})

	case ProviderBYOH:
		if in.Network != nil {
			refuse("spec.infrastructure.network", "isn't used with byoh: your machines already have their network")
		}
		const why = "isn't used with byoh: your machines already exist; list them under machines"
		eachGroup(map[string]string{"count": why, "instanceType": why, "resources": why, "disk": why})

	case ProviderAWS, ProviderGCP, ProviderAzure:
		t := cloudTerms[p]
		if n := in.Network; n != nil && (n.Addresses != "" || n.Gateway != "" || len(n.DNS) > 0) {
			refuse("spec.infrastructure.network", fmt.Sprintf("takes only subnet on %s (the %s's range); %s assigns the addresses, gateway and DNS", p, t.network, t.name))
		}
		if in.Access.SSH.Port != 0 {
			refuse("spec.infrastructure.access.ssh.port", "isn't used for machines tow creates: they listen on 22")
		}
		if k.APIEndpoint != "" {
			refuse("spec.kubernetes.apiEndpoint", fmt.Sprintf("isn't used on %s: tow creates a load balancer in front of the control planes", p))
		}
		if k.APILoadBalancer != "" {
			refuse("spec.kubernetes.apiLoadBalancer", fmt.Sprintf("isn't used on %s: tow creates a load balancer in front of the control planes", p))
		}
		eachGroup(map[string]string{
			"resources": fmt.Sprintf("isn't supported on %s yet; use instanceType (e.g. %s)", p, t.sizeExample),
			"machines":  fmt.Sprintf("isn't used on %s: tow creates the machines; use count", p),
		})

	case ProviderProxmox:
		if in.Access.SSH.Port != 0 {
			refuse("spec.infrastructure.access.ssh.port", "isn't used for machines tow creates: they listen on 22")
		}
		eachGroup(map[string]string{
			"instanceType": "isn't used on proxmox: Proxmox has no instance types; use resources (cpu, memory) and disk",
			"machines":     "isn't used on proxmox: tow creates the VMs; use count",
		})
	}
	return errs
}

// Validate keeps only real technical limits, and collects every problem so users fix their
// file in one round trip. Choices that work but look risky become warnings instead.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	s := c.Spec
	k := s.Kubernetes
	d := distributions[k.Distribution]
	c.warnings = nil

	if !dnsLabel.MatchString(c.Metadata.Name) {
		add("metadata.name %q must be a lowercase DNS label (a-z, 0-9, '-')", c.Metadata.Name)
	}

	if k.Version != "" && k.Version != VersionStable && !k8sVersion.MatchString(k.Version) {
		add("spec.kubernetes.version %q must be a release like %s, or %q for the newest (left out: %s, the one tow tests)", k.Version, DefaultKubernetesVersion, VersionStable, DefaultKubernetesVersion)
	} else if k.Distribution == DistributionKubeadm && strings.Contains(k.Version, "+k3s") {
		add("spec.kubernetes.version %q is a k3s release; kubeadm installs upstream Kubernetes (v1.X.Y)", k.Version)
	}
	if !d.RemoteNodes && c.Mode != ModeLocal {
		add("spec.kubernetes.distribution %s runs only on provider local; on machines use k3s or kubeadm", k.Distribution)
	}

	// Machines.
	cps := c.ControlPlaneCount()
	if cps < 1 {
		if c.Mode == ModeBYOH {
			add("spec.controlPlane.machines needs at least one machine")
		} else {
			add("spec.controlPlane.count must be at least 1")
		}
	}
	errs = append(errs, c.validatePools()...)
	if cps > 1 && cps%2 == 0 {
		c.warnings = append(c.warnings, fmt.Sprintf("%d control planes tolerate no more failures than %d (etcd needs a majority); use %d for high availability", cps, cps-1, cps+1))
	}
	if c.WorkerCount() == 0 && c.Mode != ModeLocal {
		c.warnings = append(c.warnings, "no workers: your workloads will run on the control planes")
	}

	// API address in front of several control planes.
	if k.APIEndpoint != "" {
		if _, _, err := net.SplitHostPort(k.APIEndpoint); err != nil {
			add("spec.kubernetes.apiEndpoint %q must be host:port, like 192.168.1.100:6443", k.APIEndpoint)
		}
	} else if cps > 1 && (c.Mode == ModeBYOH || c.Provider() == ProviderProxmox) {
		add("spec.kubernetes.apiEndpoint is required with %d control planes: a free address for the API's virtual IP (kube-vip), or your own load balancer, like 192.168.1.100:6443", cps)
	}

	for i, cidr := range s.Infrastructure.Access.AdminCIDRs {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			add("spec.infrastructure.access.adminCIDRs[%d] %q must be an address range like 203.0.113.7/32", i, cidr)
		}
	}
	seenPorts := map[int]bool{}
	for i, p := range s.Infrastructure.Access.Ports {
		switch {
		case p < 1 || p > 65535:
			add("spec.infrastructure.access.ports[%d] %d must be a TCP port, 1 to 65535", i, p)
		case seenPorts[p]:
			add("spec.infrastructure.access.ports[%d]: %d is listed twice", i, p)
		}
		seenPorts[p] = true
	}

	switch c.Provider() {
	case ProviderAWS:
		errs = append(errs, c.validateAWS()...)
	case ProviderGCP:
		errs = append(errs, c.validateGCP()...)
	case ProviderAzure:
		errs = append(errs, c.validateAzure()...)
	case ProviderProxmox:
		errs = append(errs, c.validateProxmox()...)
		errs = append(errs, c.validateCPUType()...)
	case ProviderBYOH:
		errs = append(errs, c.validateMachines()...)
	}
	errs = append(errs, c.validateOS()...)
	errs = append(errs, c.validateNetworking()...)

	// What gets installed.
	errs = append(errs, c.validateAPILoadBalancer()...)
	errs = append(errs, c.validateComponents()...)
	errs = append(errs, c.validateRuntime()...)
	errs = append(errs, c.validateCNI()...)
	errs = append(errs, c.validateStorage()...)
	errs = append(errs, c.validateBackups()...)
	errs = append(errs, c.validateServiceLB()...)
	errs = append(errs, c.validateIngress()...)
	errs = append(errs, c.validateMetricsServer()...)
	errs = append(errs, c.validateAddons()...)
	errs = append(errs, c.validateNodeSettings()...)
	c.exposedEnvWarnings()
	if k.Networking.CNI == CNINone {
		c.warnings = append(c.warnings, fmt.Sprintf("no CNI: nodes stay NotReady until one is installed\n  → set spec.kubernetes.networking.cni (%s), or install your own with spec.kubernetes.addons",
			joinOr(withoutNone(choicesFor(cniChoices, k.Distribution)))))
	}
	return errors.Join(errs...)
}

// validateComponents checks the API server's names and every component's pass-through flags.
func (c *Config) validateComponents() []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	k := c.Spec.Kubernetes

	for i, san := range k.APIServer.CertSANs {
		if !isHost(strings.TrimPrefix(san, "*.")) {
			add("spec.kubernetes.apiServer.certSANs[%d] %q must be a DNS name or an IP address", i, san)
		}
	}
	errs = append(errs, validateArgs("spec.kubernetes.apiServer.extraArgs", k.APIServer.ExtraArgs, apiServerFlags, false)...)
	errs = append(errs, validateArgs("spec.kubernetes.controllerManager.extraArgs", k.ControllerManager.ExtraArgs, controllerManagerFlags, false)...)
	errs = append(errs, validateArgs("spec.kubernetes.scheduler.extraArgs", k.Scheduler.ExtraArgs, nil, false)...)
	errs = append(errs, validateArgs("spec.kubernetes.etcd.extraArgs", k.Etcd.ExtraArgs, nil, false)...)
	errs = append(errs, validateArgs("spec.kubernetes.kubelet.extraArgs", k.Kubelet.ExtraArgs, kubeletFlags, false)...)
	// A k3s server may take the user's join tokens; k3d makes its own.
	errs = append(errs, validateArgs("spec.kubernetes.k3s.server.extraArgs", k.K3s.Server.ExtraArgs, k3sFlags, k.Distribution == DistributionK3s)...)
	errs = append(errs, validateArgs("spec.kubernetes.k3s.agent.extraArgs", k.K3s.Agent.ExtraArgs, k3sFlags, false)...)

	if !c.bundles() && (len(k.K3s.Server.ExtraArgs) > 0 || len(k.K3s.Agent.ExtraArgs) > 0) {
		add("spec.kubernetes.k3s is set but distribution is %s", k.Distribution)
	}
	if len(k.Etcd.ExtraArgs) > 0 && k.Distribution == DistributionK3d && c.ControlPlaneCount() == 1 {
		add("spec.kubernetes.etcd.extraArgs isn't used by a single k3d server: it keeps its data in SQLite, not etcd")
	}
	return errs
}

func withoutNone(list []string) []string {
	var out []string
	for _, v := range list {
		if v != CNINone {
			out = append(out, v)
		}
	}
	return out
}

// validatePools checks worker pool names and counts, and that machine names fit a hostname.
func (c *Config) validatePools() []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	seen := map[string]bool{}
	for i, w := range c.Spec.Workers {
		where := fmt.Sprintf("spec.workers[%d]", i)
		switch {
		case w.Name == "":
			add("%s.name is required when there are several worker pools", where)
		case !dnsLabel.MatchString(w.Name) || w.Name == "cp":
			add("%s.name %q must be a lowercase DNS label other than \"cp\" (it names the machines)", where, w.Name)
		case seen[w.Name]:
			add("%s.name %q is used twice", where, w.Name)
		}
		seen[w.Name] = true

		switch {
		case c.Mode == ModeBYOH && len(w.Machines) == 0:
			add("%s.machines needs at least one machine (or remove the pool)", where)
		case c.Mode != ModeBYOH && w.Count < 1:
			add("%s.count must be at least 1 (to have no workers, remove the pool)", where)
		}
	}

	// Provisioned machines are named <cluster>-<pool>-<n>, which must fit a 63-character hostname.
	if c.Mode == ModeProvision {
		longest, prefix := "cp", c.Spec.ControlPlane.Count
		for _, w := range c.Spec.Workers {
			if len(w.Name) > len(longest) {
				longest, prefix = w.Name, w.Count
			}
		}
		if n := len(c.Metadata.Name) + len(longest) + len(strconv.Itoa(max(prefix-1, 0))) + 2; n > 63 {
			add("machine names like %s-%s-%d are %d characters; hostnames allow 63: shorten metadata.name or the pool name", c.Metadata.Name, longest, max(prefix-1, 0), n)
		}
	}
	return errs
}

// validateMachines checks the byoh machine lists: every host once, an SSH user for each.
func (c *Config) validateMachines() []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	seen := map[string]string{}
	check := func(where string, ms []Machine) {
		for i, m := range ms {
			at := fmt.Sprintf("%s.machines[%d]", where, i)
			if m.Host == "" {
				add("%s.host is required", at)
				continue
			}
			if prev, dup := seen[m.Host]; dup {
				add("%s: host %q is already listed at %s", at, m.Host, prev)
			}
			seen[m.Host] = at
			if m.SSH.User == "" {
				add("%s: an SSH user is required (set spec.infrastructure.access.ssh.user, or ssh.user on the machine)", at)
			}
			if m.SSH.Port < 1 || m.SSH.Port > 65535 {
				add("%s: ssh port %d out of range", at, m.SSH.Port)
			}
		}
	}
	check("spec.controlPlane", c.Spec.ControlPlane.Machines)
	for i, w := range c.Spec.Workers {
		check(fmt.Sprintf("spec.workers[%d]", i), w.Machines)
	}
	return errs
}

func (c *Config) validateAWS() []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	a := c.Spec.Infrastructure.AWS
	if a == nil || a.Region == "" {
		add("spec.infrastructure.aws.region is required, like eu-west-3")
	}
	if n := c.Spec.Infrastructure.Network; n != nil && n.Subnet != "" {
		if p, err := netip.ParsePrefix(n.Subnet); err != nil || !p.Addr().Is4() || p.Bits() > 24 {
			add("spec.infrastructure.network.subnet %q must be an IPv4 range of /24 or larger, like 10.250.0.0/16", n.Subnet)
		}
	}
	// Instance types are checked against the distribution's minimum at apply time, through
	// the AWS API; the disk can be checked here.
	errs = append(errs, c.validateDisks()...)
	return errs
}

var (
	gcpProject = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	gcpRegion  = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+$`)
	// gcpName is what Compute Engine accepts as a resource name.
	gcpName = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
)

// gcpMaxCluster leaves room in Compute Engine's 63 characters for the longest name the module
// gives a resource after the cluster: <cluster>-api-nodes.
const gcpMaxCluster = 50

func (c *Config) validateGCP() []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	g := c.Spec.Infrastructure.GCP
	switch {
	case g == nil || g.Project == "":
		add("spec.infrastructure.gcp.project is required: the project's ID, like my-project-123456 ('gcloud projects list' shows it)")
	case !gcpProject.MatchString(g.Project):
		add("spec.infrastructure.gcp.project %q isn't a project ID: 6 to 30 lowercase letters, digits and hyphens, starting with a letter ('gcloud projects list' shows it)", g.Project)
	}
	switch {
	case g == nil || g.Region == "":
		add("spec.infrastructure.gcp.region is required, like europe-west1")
	case !gcpRegion.MatchString(g.Region):
		add("spec.infrastructure.gcp.region %q isn't a region, like europe-west1 (a zone like europe-west1-b isn't one: tow spreads the machines over the region's zones)", g.Region)
	}
	if n := c.Spec.Infrastructure.Network; n != nil && n.Subnet != "" {
		if p, err := netip.ParsePrefix(n.Subnet); err != nil || !p.Addr().Is4() || p.Bits() > 24 {
			add("spec.infrastructure.network.subnet %q must be an IPv4 range of /24 or larger, like 10.250.0.0/16", n.Subnet)
		}
	}
	// Compute Engine names start with a letter; the machines' names (63 characters at most, as
	// every hostname) are checked with every provider's.
	if name := c.Name(); !gcpName.MatchString(name) || len(name) > gcpMaxCluster {
		add("metadata.name %q: on gcp a cluster's name starts with a letter and has at most %d characters (Compute Engine names its resources after it)", name, gcpMaxCluster)
	}
	// Machine types are checked against the distribution's minimum at apply time, through
	// the Compute Engine API; the disk can be checked here.
	errs = append(errs, c.validateDisks()...)
	return errs
}

var (
	azureSubscription = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	azureLocation     = regexp.MustCompile(`^[a-z][a-z0-9]+$`)
)

func (c *Config) validateAzure() []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	a := c.Spec.Infrastructure.Azure
	switch {
	case a == nil || a.Subscription == "":
		add("spec.infrastructure.azure.subscription is required: the subscription's ID ('az account show --query id' prints it)")
	case !azureSubscription.MatchString(a.Subscription):
		add("spec.infrastructure.azure.subscription %q isn't a subscription ID, a GUID like 00000000-0000-0000-0000-000000000000 ('az account show --query id' prints it)", a.Subscription)
	}
	switch {
	case a == nil || a.Location == "":
		add("spec.infrastructure.azure.location is required, like westeurope")
	case !azureLocation.MatchString(a.Location):
		add("spec.infrastructure.azure.location %q isn't a location's name, like westeurope ('az account list-locations --query [].name' lists them)", a.Location)
	}
	if n := c.Spec.Infrastructure.Network; n != nil && n.Subnet != "" {
		if p, err := netip.ParsePrefix(n.Subnet); err != nil || !p.Addr().Is4() || p.Bits() > 24 {
			add("spec.infrastructure.network.subnet %q must be an IPv4 range of /24 or larger, like 10.250.0.0/16", n.Subnet)
		}
	}
	// VM sizes are checked against the distribution's minimum at apply time, through Azure's
	// API; the disk can be checked here.
	errs = append(errs, c.validateDisks()...)
	return errs
}

// validateDisks checks every group's disk against the distribution's minimum for its role.
func (c *Config) validateDisks() []error {
	var errs []error
	for _, g := range c.namedGroups() {
		if g.Disk == "" {
			continue
		}
		gb, err := GiB(g.Disk)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s.disk: %v", g.path, err))
			continue
		}
		if m, ok := MinimumFor(c.Distribution(), g.role); ok && gb < m.MinDiskGB() {
			errs = append(errs, fmt.Errorf("%s.disk is %dGi; a %s needs at least %dGi", g.path, gb, m.What, m.MinDiskGB()))
		}
	}
	return errs
}

type namedGroup struct {
	*Group
	path, role string
}

func (c *Config) namedGroups() []namedGroup {
	out := []namedGroup{{&c.Spec.ControlPlane.Group, "spec.controlPlane", RoleControlPlane}}
	for i := range c.Spec.Workers {
		out = append(out, namedGroup{&c.Spec.Workers[i].Group, fmt.Sprintf("spec.workers[%d]", i), RoleWorker})
	}
	return out
}

// DefaultCloudNetwork is the range of the network the public clouds' modules create (AWS's
// VPC, GCP's subnet) when the file sets none.
const DefaultCloudNetwork = "10.250.0.0/16"

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
