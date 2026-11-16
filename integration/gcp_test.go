package integration

import (
	"os"
	"testing"
	"time"

	"github.com/towproj/tow/internal/config"
)

// The GCP tests run in the project TOW_TEST_GCP_PROJECT names (with the Compute Engine and
// IAM APIs on, and your Application Default Credentials), in TOW_TEST_GCP_REGION
// (europe-west1 by default). A free trial allows 4 public addresses: one per machine, one per
// API load balancer, so the tests stay within it.

// TestGCPInfra builds a real kubeadm cluster on GCP through 'apply' (the dev-test flavor: one
// control plane, one worker, Calico, two e2-medium) and runs every check of runCloud on it.
// TOW_TEST_OS picks the system (almalinux-9, almalinux-10); TOW_TEST_KUBERNETES adds to
// spec.kubernetes, as on AWS; TOW_TEST_GCP_CONFIG builds a cluster file instead (its own
// project and region: examples/pd-gcp.yaml for persistent disks).
//
// Cost: two e2-medium with their disks and public addresses for ~15 minutes, about $0.02.
func TestGCPInfra(t *testing.T) {
	if os.Getenv("INTEGRATION_GCP") == "" {
		t.Skip("set INTEGRATION_GCP=1 and TOW_TEST_GCP_PROJECT to create real GCP machines")
	}
	runCloud(t, cloudRun{where: gcpTarget(t), prefix: "test-gcp", controlPlanes: 1, workers: 1, deadline: 50 * time.Minute, configEnv: "TOW_TEST_GCP_CONFIG"})
}

// TestGCPHA builds three control planes, one per zone, behind the two passthrough load
// balancers tow creates (no worker: three machines and the external load balancer are a
// free trial's 4 public addresses; the control planes run the workloads), runs every check of
// runCloud, including the API name each machine resolves in /etc/hosts, then takes the first
// control plane away: the API through the load balancer must still answer and run a new hello
// world, and the machine must be Ready again once started. kubeadm by default;
// TOW_TEST_KUBERNETES='{distribution: k3s}' tries k3s.
//
// Cost: three e2-medium and two load balancers' forwarding rules for ~30 minutes, about $0.06.
// The load balancers don't go away by themselves: if this process dies before cleaning up, run
// the 'tow delete' the last lines name.
func TestGCPHA(t *testing.T) {
	if os.Getenv("INTEGRATION_GCP_HA") == "" {
		t.Skip("set INTEGRATION_GCP_HA=1 and TOW_TEST_GCP_PROJECT to create a real HA cluster on GCP")
	}
	runCloud(t, cloudRun{where: gcpTarget(t), prefix: "test-gcp-ha", controlPlanes: 3, schedulable: true, deadline: 50 * time.Minute, failover: true})
}

// gcpTarget is where the GCP tests run; the project is the user's own, never written here.
func gcpTarget(t *testing.T) target {
	t.Helper()
	project := os.Getenv("TOW_TEST_GCP_PROJECT")
	if project == "" {
		t.Fatal("TOW_TEST_GCP_PROJECT must name the GCP project to test in")
	}
	return target{cloud: config.ProviderGCP, region: envOr("TOW_TEST_GCP_REGION", "europe-west1"), project: project}
}
