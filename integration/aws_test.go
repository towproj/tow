package integration

import (
	"os"
	"testing"
	"time"

	"github.com/towproj/tow/internal/config"
)

// TestAWSInfra builds a real kubeadm cluster through 'apply' (the dev-test flavor: one control
// plane, one worker, Calico) and runs every check of runCloud on it.
//
// TOW_TEST_KUBERNETES is YAML for spec.kubernetes on top of the flavor's, to try other
// choices: '{containerRuntime: cri-o, networking: {cni: flannel}}', for instance.
// TOW_TEST_AWS_CONFIG builds a cluster file instead (examples/storage-aws.yaml: three
// t3.small with every storage driver, ~25 minutes, about $0.04).
//
// Cost: two t3.small (the smallest size kubeadm accepts) for ~10 minutes, about $0.03.
func TestAWSInfra(t *testing.T) {
	if os.Getenv("INTEGRATION_AWS") == "" {
		t.Skip("set INTEGRATION_AWS=1 to create real AWS servers")
	}
	runCloud(t, cloudRun{where: awsTarget(), prefix: "test-aws", controlPlanes: 1, workers: 1, deadline: 50 * time.Minute, kubeadmOnly: true, // 50: room for cnpg-aws.yaml's four machines and failover
		configEnv: "TOW_TEST_AWS_CONFIG"})
}

// TestAWSHA builds a cluster of three control planes and a worker behind the load balancer
// tow creates (an NLB), runs every check TestAWSInfra does, then takes the first control
// plane away: it's stopped through AWS, and the API, through the load balancer, must still
// answer and run a new hello world. Then it's started again and must be Ready. kubeadm by
// default; TOW_TEST_KUBERNETES='{distribution: k3s}' tries k3s.
//
// Cost: four t3.small and two NLBs (a public one for this machine, an internal one for the
// nodes) for ~25 minutes, about $0.07. The NLBs don't destroy themselves: if this process
// dies before cleaning up, run the 'tow delete' the last lines name.
func TestAWSHA(t *testing.T) {
	if os.Getenv("INTEGRATION_AWS_HA") == "" {
		t.Skip("set INTEGRATION_AWS_HA=1 to create a real HA cluster on AWS (an NLB, about $0.06)")
	}
	runCloud(t, cloudRun{where: awsTarget(), prefix: "test-aws-ha", controlPlanes: 3, workers: 1, deadline: 45 * time.Minute, failover: true})
}

// awsTarget is where the AWS tests run: TOW_TEST_REGION, eu-west-3 by default.
func awsTarget() target {
	return target{cloud: config.ProviderAWS, region: envOr("TOW_TEST_REGION", "eu-west-3")}
}
