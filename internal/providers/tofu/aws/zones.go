package aws

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/towproj/tow/internal/providers/tofu/cloud"
)

// place puts each machine in a zone and returns the cluster's zones (cloud.Placement): each
// zone has its subnet.
func (c Cloud) place(ctx context.Context, region string, machines []cloud.Machine, prev *cloud.Inputs) ([]string, error) {
	p := cloud.Placement{Region: region, DefaultSize: DefaultInstanceType, Kind: "instance type",
		Offerings: func(ctx context.Context, types []string) (map[string][]string, error) {
			return c.offerings(ctx, region, types)
		}}
	return p.Place(ctx, machines, prev)
}

// offerings lists, for each instance type, the zones of region that offer it.
func (c Cloud) offerings(ctx context.Context, region string, types []string) (map[string][]string, error) {
	cfg, err := c.config(ctx, region)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	pages := ec2.NewDescribeInstanceTypeOfferingsPaginator(ec2.NewFromConfig(cfg), &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: ec2types.LocationTypeAvailabilityZone,
		Filters:      []ec2types.Filter{{Name: aws.String("instance-type"), Values: types}},
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("find the zones of %s offering %s: %w", region, strings.Join(types, ", "), err)
		}
		for _, o := range page.InstanceTypeOfferings {
			t := string(o.InstanceType)
			out[t] = append(out[t], aws.ToString(o.Location))
		}
	}
	return out, nil
}
