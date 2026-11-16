package integration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/providers/tofu/aws"
	"github.com/towproj/tow/internal/providers/tofu/azure"
	"github.com/towproj/tow/internal/providers/tofu/cloud"
	"github.com/towproj/tow/internal/providers/tofu/gcp"
)

// exitMachinesUp ends a run that may leave cloud machines running, whether its checks passed
// or not: it's a warning with something to do, not a test failure. 'go test' turns every
// non-zero exit into 1, so the cloud make targets run the test binary itself to keep it.
const exitMachinesUp = 3

var (
	leftMu sync.Mutex
	left   []leftover
)

// target is where a cloud test's machines run: an AWS region, a GCP project's region, or an
// Azure subscription's location.
type target struct {
	cloud, region, project, subscription string
}

func (g target) name() string { return config.CloudName(g.cloud) }

// notTerminated lists the cluster's machines the cloud doesn't report gone yet.
func (g target) notTerminated(ctx context.Context, cluster string) ([]cloud.Instance, error) {
	switch g.cloud {
	case config.ProviderGCP:
		return gcp.Cloud{}.NotTerminated(ctx, g.project, cluster)
	case config.ProviderAzure:
		return azure.Cloud{}.NotTerminated(ctx, g.subscription, cluster)
	}
	return aws.Cloud{}.NotTerminated(ctx, g.region, cluster)
}

// leftover is a cluster whose machines the cloud didn't report gone in time.
type leftover struct {
	cluster   string
	where     target
	instances []cloud.Instance
	deleteErr string // tow's delete failed
	queryErr  string // the cloud couldn't be asked
}

func TestMain(m *testing.M) {
	code := m.Run()
	leftMu.Lock()
	ls := left
	leftMu.Unlock()
	if len(ls) == 0 {
		os.Exit(code)
	}
	result := "the checks passed"
	if code != 0 {
		result = "the checks failed too (see above)"
	}
	var b strings.Builder
	b.WriteString("\nWARNING: cloud machines from this run may still be running (they go away by themselves within an hour or two anyway):\n")
	for _, l := range ls {
		b.WriteString(l.describe())
	}
	fmt.Fprintf(&b, "Exit code %d: %s, but go and shut these machines down.\n", exitMachinesUp, result)
	fmt.Fprint(os.Stderr, b.String())
	os.Exit(exitMachinesUp)
}

// describe says what's left and how to shut it down.
func (l leftover) describe() string {
	var b strings.Builder
	w := l.where
	switch w.cloud {
	case config.ProviderGCP:
		fmt.Fprintf(&b, "  cluster %s in GCP project %s, %s:\n", l.cluster, w.project, w.region)
	case config.ProviderAzure:
		fmt.Fprintf(&b, "  cluster %s in Azure subscription %s, %s (resource group %s):\n", l.cluster, w.subscription, w.region, azure.ResourceGroup(l.cluster))
	default:
		fmt.Fprintf(&b, "  cluster %s in %s:\n", l.cluster, w.region)
	}
	if l.deleteErr != "" {
		fmt.Fprintf(&b, "    delete failed: %s\n    → tow delete --name %s --yes (its state is kept in ~/.tow/clusters/%s)\n", l.deleteErr, l.cluster, l.cluster)
	}
	if len(l.instances) > 0 {
		var ids, seen []string
		for _, i := range l.instances {
			ids = append(ids, i.ID)
			seen = append(seen, i.ID+" ("+i.State+")")
		}
		fmt.Fprintf(&b, "    machines not terminated: %s\n", strings.Join(seen, ", "))
		switch w.cloud {
		case config.ProviderGCP:
			fmt.Fprintf(&b, "    → gcloud compute instances delete %s --project %s (it asks each one's zone)\n", strings.Join(ids, " "), w.project)
		case config.ProviderAzure:
			fmt.Fprintf(&b, "    → az group delete --name %s --subscription %s --yes (everything of the cluster's is in it)\n", azure.ResourceGroup(l.cluster), w.subscription)
		default:
			fmt.Fprintf(&b, "    → aws ec2 terminate-instances --region %s --instance-ids %s\n", w.region, strings.Join(ids, " "))
		}
	}
	if l.queryErr != "" {
		fmt.Fprintf(&b, "    %s couldn't be asked: %s\n", w.name(), l.queryErr)
		switch w.cloud {
		case config.ProviderGCP:
			fmt.Fprintf(&b, "    → gcloud compute instances list --project %s --filter='labels.%s=%s'\n", w.project, cloud.TagCluster, l.cluster)
		case config.ProviderAzure:
			fmt.Fprintf(&b, "    → az vm list --resource-group %s --subscription %s --output table\n", azure.ResourceGroup(l.cluster), w.subscription)
		default:
			fmt.Fprintf(&b, "    → aws ec2 describe-instances --region %s --filters Name=tag:%s,Values=%s Name=instance-state-name,Values=pending,running,shutting-down,stopping,stopped --query 'Reservations[].Instances[].InstanceId'\n",
				w.region, cloud.TagCluster, l.cluster)
		}
	}
	return b.String()
}

// The warning names every machine left and the command that shuts it down. No cloud needed.
func TestLeftoverSaysWhatToRun(t *testing.T) {
	aws := target{cloud: config.ProviderAWS, region: "eu-west-3"}
	got := leftover{cluster: "test-aws-1", where: aws, deleteErr: "tofu destroy: timeout",
		instances: []cloud.Instance{{ID: "i-0abc", State: "running"}, {ID: "i-0def", State: "shutting-down"}}}.describe()
	for _, want := range []string{
		"cluster test-aws-1 in eu-west-3:", "delete failed: tofu destroy: timeout", "tow delete --name test-aws-1 --yes",
		"machines not terminated: i-0abc (running), i-0def (shutting-down)",
		"aws ec2 terminate-instances --region eu-west-3 --instance-ids i-0abc i-0def",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("warning lacks %q:\n%s", want, got)
		}
	}
	if got := (leftover{cluster: "c", where: aws, queryErr: "no credentials"}).describe(); !strings.Contains(got, "AWS couldn't be asked") || !strings.Contains(got, "Name=tag:tow-cluster,Values=c") {
		t.Errorf("without AWS's answer, say how to look:\n%s", got)
	}

	az := target{cloud: config.ProviderAzure, region: "westeurope", subscription: "sub-1"}
	got = leftover{cluster: "test-az-1", where: az, instances: []cloud.Instance{{ID: "test-az-1-cp-0", State: "running"}}}.describe()
	for _, want := range []string{"cluster test-az-1 in Azure subscription sub-1, westeurope (resource group tow-test-az-1):", "az group delete --name tow-test-az-1 --subscription sub-1 --yes"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning lacks %q:\n%s", want, got)
		}
	}

	gcp := target{cloud: config.ProviderGCP, region: "europe-west1", project: "demo-project"}
	got = leftover{cluster: "test-gcp-1", where: gcp, instances: []cloud.Instance{{ID: "test-gcp-1-cp-0", State: "running"}}, queryErr: "403"}.describe()
	for _, want := range []string{
		"cluster test-gcp-1 in GCP project demo-project, europe-west1:", "gcloud compute instances delete test-gcp-1-cp-0 --project demo-project",
		"GCP couldn't be asked: 403", "gcloud compute instances list --project demo-project --filter='labels.tow-cluster=test-gcp-1'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("warning lacks %q:\n%s", want, got)
		}
	}
}

// confirmTerminated runs last in a cloud test, once its cluster was deleted (deleteErr: how
// that went). It waits until the cloud reports every machine of the cluster gone, which a
// clean delete reaches within a few minutes. Machines still up, or a failed delete, don't fail
// the test: they're logged, and the run ends with exit code 3 and the commands to run.
func confirmTerminated(t *testing.T, where target, cluster string, deleteErr error) {
	t.Helper()
	wait := 5 * time.Minute
	if deleteErr != nil {
		wait = 0 // nothing is shutting them down: ask once
	}
	deadline := time.Now().Add(wait)
	var insts []cloud.Instance
	var err error
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		insts, err = where.notTerminated(ctx, cluster)
		cancel()
		if (err == nil && len(insts) == 0) || time.Now().After(deadline) {
			break
		}
		time.Sleep(15 * time.Second)
	}
	if err == nil && len(insts) == 0 && deleteErr == nil {
		t.Logf("%s: %s reports every machine gone", cluster, where.name())
		return
	}
	l := leftover{cluster: cluster, where: where, instances: insts}
	if deleteErr != nil {
		l.deleteErr = deleteErr.Error()
	}
	if err != nil {
		l.queryErr = err.Error()
	}
	t.Logf("WARNING (exit code %d at the end, not a failure):\n%s", exitMachinesUp, l.describe())
	leftMu.Lock()
	left = append(left, l)
	leftMu.Unlock()
}
