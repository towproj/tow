// Package aws is the AWS provision target: its OpenTofu module plus the AWS API calls
// OpenTofu doesn't make. It is the only package allowed to import the AWS SDK.
package aws

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	elb "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/providers/tofu/cloud"
)

// Hidden files (.terraform from local runs) are excluded by go:embed, except the lock file,
// named: it pins the provider release tow is tested with, and lets 'tofu init' reuse
// the shared provider cache without writing it again.
//
//go:embed module module/.terraform.lock.hcl
var module embed.FS

// Cloud uses the same credential chain as OpenTofu's AWS provider (env vars, profiles
// and SSO, web identity, instance roles), so whatever works for one works for the other.
type Cloud struct{}

const credentialHelp = `Any of these works:
  - on your machine: 'aws sso login' or 'aws configure' (choose a profile with AWS_PROFILE=<name>)
  - as a service account or in CI: AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, or a role via AWS_ROLE_ARN and AWS_WEB_IDENTITY_TOKEN_FILE (OIDC)
  - on an EC2 instance: an attached instance role`

func (Cloud) Module() fs.FS {
	sub, err := fs.Sub(module, "module")
	if err != nil {
		panic(err) // the directory is embedded at build time
	}
	return sub
}

// DefaultInstanceType sizes pools that name no instance type.
const DefaultInstanceType = "t3.medium"

// instanceType is the type a machine or pool gets: its own, or the default.
func instanceType(t string) string {
	if t != "" {
		return t
	}
	return DefaultInstanceType
}

func (Cloud) MachineType() string { return "aws_instance" }

// AdminRules: the security group's rules for the admin addresses, one per address (and port
// or protocol). The rule between the nodes isn't one of them.
func (Cloud) AdminRules() []string {
	return []string{
		"aws_vpc_security_group_ingress_rule.ssh",
		"aws_vpc_security_group_ingress_rule.api",
		"aws_vpc_security_group_ingress_rule.node_ports",
		"aws_vpc_security_group_ingress_rule.open_ports",
	}
}

// Vars places each machine in a zone, and returns the zones, the default instance type, the
// machines' operating system, and the VPC range when the file sets one.
func (c Cloud) Vars(ctx context.Context, spec providers.Spec, machines []cloud.Machine, prev *cloud.Inputs) (map[string]any, error) {
	zones, err := c.place(ctx, spec.Machines.Region, machines, prev)
	if err != nil {
		return nil, err
	}
	os := spec.Machines.OS
	if os == "" {
		os = config.DefaultOS
	}
	vars := map[string]any{"zones": zones, "default_instance_type": DefaultInstanceType, "ebs_csi": slices.Contains(spec.Storage, "aws-ebs"), "os": os}
	if n := spec.Machines.Network; n != nil && n.Subnet != "" {
		vars["vpc_cidr"] = n.Subnet
	}
	return vars, nil
}

// BeforeCreate checks each pool's instance type against what the distribution needs for the
// pool's role (CPUs, memory, CPU architecture), before anything is created.
func (c Cloud) BeforeCreate(ctx context.Context, spec providers.Spec, _ []cloud.Machine) error {
	m := spec.Machines
	typeOf := func(p providers.Pool) string { return instanceType(p.InstanceType) }
	var names []ec2types.InstanceType
	seen := map[string]bool{}
	for _, p := range m.Pools {
		if t := typeOf(p); !seen[t] {
			seen[t] = true
			names = append(names, ec2types.InstanceType(t))
		}
	}

	cfg, err := c.config(ctx, m.Region)
	if err != nil {
		return err
	}
	out, err := ec2.NewFromConfig(cfg).DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{InstanceTypes: names})
	if err != nil {
		return fmt.Errorf("check instance types %v in %s (is each one spelled right and offered there?): %w", names, m.Region, err)
	}
	info := map[string]ec2types.InstanceTypeInfo{}
	for _, t := range out.InstanceTypes {
		info[string(t.InstanceType)] = t
	}

	var problems []string
	for _, p := range m.Pools {
		t := typeOf(p)
		min, ok := cloud.Minimum(spec.Engine, p.Role, m.OS)
		it, found := info[t]
		if !ok || !found {
			continue
		}
		var cpus, memMiB int
		if it.VCpuInfo != nil {
			cpus = int(aws.ToInt32(it.VCpuInfo.DefaultVCpus))
		}
		if it.MemoryInfo != nil {
			memMiB = int(aws.ToInt64(it.MemoryInfo.SizeInMiB))
		}
		if cpus < min.CPUs || memMiB < min.MemoryMB {
			problems = append(problems, fmt.Sprintf("pool %s: %s has %d vCPUs and %d MiB; a %s needs at least %d vCPUs and %d MiB",
				p.Name, t, cpus, memMiB, min.What, min.CPUs, min.MemoryMB))
		}
		if it.ProcessorInfo != nil && !archSupported(it.ProcessorInfo.SupportedArchitectures, min.Archs) {
			problems = append(problems, fmt.Sprintf("pool %s: %s's CPU architecture %v has no %s builds", p.Name, t, it.ProcessorInfo.SupportedArchitectures, min.What))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("instance types too small or unsupported:\n  - %s\nPick bigger instanceType values", strings.Join(problems, "\n  - "))
	}
	return nil
}

// archSupported matches AWS architecture names (x86_64, arm64) with uname -m names (x86_64, aarch64).
func archSupported(have []ec2types.ArchitectureType, want []string) bool {
	for _, h := range have {
		name := string(h)
		if name == "arm64" {
			name = "aarch64"
		}
		for _, w := range want {
			if name == w {
				return true
			}
		}
	}
	return false
}

func (Cloud) config(ctx context.Context, region string) (aws.Config, error) {
	return awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
}

func (c Cloud) Identity(ctx context.Context, m *providers.MachineRequest) (string, error) {
	cfg, err := c.config(ctx, m.Region)
	if err == nil {
		var out *sts.GetCallerIdentityOutput
		if out, err = sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}); err == nil {
			return fmt.Sprintf("AWS account %s as %s", aws.ToString(out.Account), aws.ToString(out.Arn)), nil
		}
	}
	return "", fmt.Errorf("could not verify AWS credentials: %w\n%s", err, credentialHelp)
}

// AdminCIDR is this machine's public address: the servers have public IPs, so that is the
// address they see.
func (Cloud) AdminCIDR(ctx context.Context, _ *providers.MachineRequest) (string, error) {
	return cloud.PublicCIDR(ctx)
}

func (c Cloud) Instances(ctx context.Context, m *providers.MachineRequest, cluster string) ([]cloud.Instance, error) {
	cfg, err := c.config(ctx, m.Region)
	if err != nil {
		return nil, err
	}
	return liveInstances(ctx, ec2.NewFromConfig(cfg), cluster)
}

func clusterFilter(cluster string) ec2types.Filter {
	return ec2types.Filter{Name: aws.String("tag:" + cloud.TagCluster), Values: []string{cluster}}
}

// NotTerminated lists the cluster's machines AWS doesn't report as terminated yet, those
// still shutting down included: what the end-to-end tests check after deleting a cluster.
func (c Cloud) NotTerminated(ctx context.Context, region, cluster string) ([]cloud.Instance, error) {
	cfg, err := c.config(ctx, region)
	if err != nil {
		return nil, err
	}
	return instances(ctx, ec2.NewFromConfig(cfg), cluster, "pending", "running", "shutting-down", "stopping", "stopped")
}

// PowerOff stops the cluster's machine called name and waits until it's stopped; PowerOn
// starts it, waits until it runs, and returns its public address, a new one: AWS gives a
// machine another public IP each time it starts. Tests take a control plane away with them.
func (c Cloud) PowerOff(ctx context.Context, region, cluster, name string) error {
	_, err := c.power(ctx, region, cluster, name, false)
	return err
}

func (c Cloud) PowerOn(ctx context.Context, region, cluster, name string) (publicIP string, err error) {
	return c.power(ctx, region, cluster, name, true)
}

func (c Cloud) power(ctx context.Context, region, cluster, name string, on bool) (string, error) {
	cfg, err := c.config(ctx, region)
	if err != nil {
		return "", err
	}
	client := ec2.NewFromConfig(cfg)
	out, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{
		clusterFilter(cluster),
		{Name: aws.String("tag:Name"), Values: []string{name}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
	}})
	if err != nil {
		return "", err
	}
	var ids []string
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			ids = append(ids, aws.ToString(i.InstanceId))
		}
	}
	if len(ids) != 1 {
		return "", fmt.Errorf("cluster %s has %d machines called %s", cluster, len(ids), name)
	}
	byID := &ec2.DescribeInstancesInput{InstanceIds: ids}
	if !on {
		if _, err := client.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: ids}); err != nil {
			return "", err
		}
		return "", ec2.NewInstanceStoppedWaiter(client).Wait(ctx, byID, 10*time.Minute)
	}
	if _, err := client.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: ids}); err != nil {
		return "", err
	}
	running, err := ec2.NewInstanceRunningWaiter(client).WaitForOutput(ctx, byID, 10*time.Minute)
	if err != nil {
		return "", err
	}
	for _, r := range running.Reservations {
		for _, i := range r.Instances {
			if ip := aws.ToString(i.PublicIpAddress); ip != "" {
				return ip, nil
			}
		}
	}
	return "", fmt.Errorf("%s runs again but has no public address", name)
}

// liveInstances skips terminated machines: AWS keeps listing them for about an hour.
func liveInstances(ctx context.Context, client *ec2.Client, cluster string) ([]cloud.Instance, error) {
	return instances(ctx, client, cluster, "pending", "running", "stopping", "stopped")
}

// instances lists the cluster's machines in the given states.
func instances(ctx context.Context, client *ec2.Client, cluster string, states ...string) ([]cloud.Instance, error) {
	var out []cloud.Instance
	pages := ec2.NewDescribeInstancesPaginator(client, &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{
		clusterFilter(cluster),
		{Name: aws.String("instance-state-name"), Values: states},
	}})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range page.Reservations {
			for _, i := range r.Instances {
				inst := cloud.Instance{ID: aws.ToString(i.InstanceId)}
				if i.State != nil {
					inst.State = string(i.State.Name)
				}
				for _, t := range i.Tags {
					if aws.ToString(t.Key) == cloud.TagRole {
						inst.Role = aws.ToString(t.Value)
					}
				}
				out = append(out, inst)
			}
		}
	}
	return out, nil
}

// Leftovers checks every resource type the module creates. Each list call is filtered by the
// cluster tag except load balancers, whose API has no tag filter.
func (c Cloud) Leftovers(ctx context.Context, m *providers.MachineRequest, cluster string) ([]string, error) {
	cfg, err := c.config(ctx, m.Region)
	if err != nil {
		return nil, err
	}
	client := ec2.NewFromConfig(cfg)
	byTag := []ec2types.Filter{clusterFilter(cluster)}

	var left []string
	add := func(kind string, ids ...string) {
		for _, id := range ids {
			left = append(left, kind+" "+id)
		}
	}

	insts, err := liveInstances(ctx, client, cluster)
	if err != nil {
		return nil, err
	}
	for _, i := range insts {
		add("instance", i.ID)
	}

	vols, err := client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{Filters: append(byTag,
		ec2types.Filter{Name: aws.String("status"), Values: []string{"creating", "available", "in-use"}})})
	if err != nil {
		return nil, err
	}
	for _, v := range vols.Volumes {
		add("volume", aws.ToString(v.VolumeId))
	}

	keys, err := client.DescribeKeyPairs(ctx, &ec2.DescribeKeyPairsInput{Filters: byTag})
	if err != nil {
		return nil, err
	}
	for _, k := range keys.KeyPairs {
		add("key pair", aws.ToString(k.KeyName))
	}

	sgs, err := client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{Filters: byTag})
	if err != nil {
		return nil, err
	}
	for _, g := range sgs.SecurityGroups {
		add("security group", aws.ToString(g.GroupId))
	}

	subnets, err := client.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{Filters: byTag})
	if err != nil {
		return nil, err
	}
	for _, s := range subnets.Subnets {
		add("subnet", aws.ToString(s.SubnetId))
	}

	rts, err := client.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{Filters: byTag})
	if err != nil {
		return nil, err
	}
	for _, r := range rts.RouteTables {
		add("route table", aws.ToString(r.RouteTableId))
	}

	igws, err := client.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{Filters: byTag})
	if err != nil {
		return nil, err
	}
	for _, g := range igws.InternetGateways {
		add("internet gateway", aws.ToString(g.InternetGatewayId))
	}

	vpcs, err := client.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{Filters: byTag})
	if err != nil {
		return nil, err
	}
	for _, v := range vpcs.Vpcs {
		add("vpc", aws.ToString(v.VpcId))
	}

	lbs, err := taggedLoadBalancing(ctx, elb.NewFromConfig(cfg), cluster)
	if err != nil {
		return nil, err
	}
	return append(left, lbs...), nil
}

func taggedLoadBalancing(ctx context.Context, client *elb.Client, cluster string) ([]string, error) {
	var arns []string
	lbPages := elb.NewDescribeLoadBalancersPaginator(client, &elb.DescribeLoadBalancersInput{})
	for lbPages.HasMorePages() {
		page, err := lbPages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, lb := range page.LoadBalancers {
			arns = append(arns, aws.ToString(lb.LoadBalancerArn))
		}
	}
	tgPages := elb.NewDescribeTargetGroupsPaginator(client, &elb.DescribeTargetGroupsInput{})
	for tgPages.HasMorePages() {
		page, err := tgPages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, tg := range page.TargetGroups {
			arns = append(arns, aws.ToString(tg.TargetGroupArn))
		}
	}

	var left []string
	for start := 0; start < len(arns); start += 20 { // DescribeTags takes at most 20 ARNs
		batch := arns[start:min(start+20, len(arns))]
		out, err := client.DescribeTags(ctx, &elb.DescribeTagsInput{ResourceArns: batch})
		if err != nil {
			return nil, err
		}
		for _, d := range out.TagDescriptions {
			for _, t := range d.Tags {
				if aws.ToString(t.Key) != cloud.TagCluster || aws.ToString(t.Value) != cluster {
					continue
				}
				kind := "target group"
				if strings.Contains(aws.ToString(d.ResourceArn), ":loadbalancer/") {
					kind = "load balancer"
				}
				left = append(left, kind+" "+aws.ToString(d.ResourceArn))
			}
		}
	}
	return left, nil
}
