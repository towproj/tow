package integration

import (
	"os"
	"testing"
	"time"

	"github.com/towproj/tow/internal/config"
)

// The Azure tests run in the subscription TOW_TEST_AZURE_SUBSCRIPTION names (your az login,
// or a service principal), in TOW_TEST_AZURE_LOCATION (swedencentral by default: westeurope takes no new customers). Everything
// of a test cluster's is in its resource group, tow-<cluster>.

// TestAzureInfra builds a real kubeadm cluster on Azure through 'apply' (the dev-test flavor: one
// control plane, one worker, Calico, two Standard_B2s_v2) and runs every check of runCloud on it.
// TOW_TEST_OS picks the system (almalinux-9, almalinux-10); TOW_TEST_KUBERNETES adds to
// spec.kubernetes, as on AWS; TOW_TEST_AZURE_CONFIG builds a cluster file instead (its own
// subscription and location: examples/disk-azure.yaml for managed disks).
//
// Cost: two Standard_B2s_v2 with their disks and public addresses for ~15 minutes, about $0.03.
func TestAzureInfra(t *testing.T) {
	if os.Getenv("INTEGRATION_AZURE") == "" {
		t.Skip("set INTEGRATION_AZURE=1 and TOW_TEST_AZURE_SUBSCRIPTION to create real Azure VMs")
	}
	runCloud(t, cloudRun{where: azureTarget(t), prefix: "test-az", controlPlanes: 1, workers: 1, deadline: 50 * time.Minute, configEnv: "TOW_TEST_AZURE_CONFIG"})
}

// TestAzureHA builds three control planes, one per zone, and a worker behind the two Standard
// load balancers tow creates, runs every check of runCloud, including the API name each
// machine resolves in /etc/hosts, then deallocates the first control plane: the API through
// the load balancer must still answer and run a new hello world, and the machine must be
// Ready again once started. kubeadm by default; TOW_TEST_KUBERNETES='{distribution: k3s}'
// tries k3s.
//
// Cost: four Standard_B2s_v2 and two load balancers for ~30 minutes, about $0.08.
func TestAzureHA(t *testing.T) {
	if os.Getenv("INTEGRATION_AZURE_HA") == "" {
		t.Skip("set INTEGRATION_AZURE_HA=1 and TOW_TEST_AZURE_SUBSCRIPTION to create a real HA cluster on Azure")
	}
	runCloud(t, cloudRun{where: azureTarget(t), prefix: "test-az-ha", controlPlanes: 3, workers: 1, deadline: 50 * time.Minute, failover: true})
}

// azureTarget is where the Azure tests run; the subscription is the user's own, never written
// here.
func azureTarget(t *testing.T) target {
	t.Helper()
	sub := os.Getenv("TOW_TEST_AZURE_SUBSCRIPTION")
	if sub == "" {
		t.Fatal("TOW_TEST_AZURE_SUBSCRIPTION must name the Azure subscription to test in")
	}
	return target{cloud: config.ProviderAzure, region: envOr("TOW_TEST_AZURE_LOCATION", "swedencentral"), subscription: sub}
}
