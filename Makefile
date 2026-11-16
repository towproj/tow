.PHONY: test test-integration test-aws test-aws-ha test-aws-lifecycle test-byoh-aws test-gcp test-gcp-ha test-gcp-lifecycle test-azure test-azure-ha test-azure-lifecycle test-proxmox test-lifecycle test-lifecycle-conformance test-proxmox-all test-conformance build clean hooks secrets lint cover

test:
	go test -short ./...

# Real local k3d clusters, free: one bare k3d cluster, then one built from each of
# examples/custom-bare.yaml, local.yaml and local-helm.yaml, and one with a tainted worker.
# KUBECONFIG is a temporary file. Also downloads the pinned helm and tofu, and kubectl, into
# throwaway folders.
test-integration:
	INTEGRATION=1 go test -v -count=1 -timeout 35m -run K3d ./integration/...
	INTEGRATION=1 go test -v -count=1 -run TestRealDownloads ./internal/tools/

# The AWS tests build the test binary and run it themselves: 'go test' turns every failing
# exit code into 1, and these have one more. Exit code 3 (make says "Error 3") means AWS may
# still have machines from the run up, whether the checks passed or not; the end of the
# output names them and how to shut them down. 1 is a failed check, 0 a clean run.

# Builds a kubeadm cluster on two real t3.small AWS servers, checks it, deletes it (~$0.03 a
# run). Region: TOW_TEST_REGION (default eu-west-3); other Kubernetes choices:
# TOW_TEST_KUBERNETES; a cluster file instead: TOW_TEST_AWS_CONFIG, like
# ../examples/storage-aws.yaml (~$0.04) (docs/testing). The timeout leaves room for the
# test's 40 min deadline plus cleanup.
test-aws:
	cd integration && go test -c -o integration.test . && INTEGRATION_AWS=1 ./integration.test -test.v -test.timeout 75m -test.run '^TestAWSInfra$$'

# Opt-in: three control planes and a worker behind an NLB, then the first control plane is
# stopped and must not be missed (~$0.07 a run; NLBs aren't in AWS's free tier). k3s instead
# of kubeadm: TOW_TEST_KUBERNETES='{distribution: k3s}'. The timeout leaves room for the
# test's 45 min deadline plus cleanup.
test-aws-ha:
	cd integration && go test -c -o integration.test . && INTEGRATION_AWS_HA=1 ./integration.test -test.v -test.timeout 70m -test.run '^TestAWSHA$$'

# Checks two throwaway AWS servers as if they were your own machines (~$0.05 a run): k3s,
# then kubeadm, each deleted with --reset. Only the byoh checks run; the servers come from the
# OpenTofu module, which test-aws tests on its own. The timeout leaves room for the test's
# 50 min deadline plus cleanup.
test-byoh-aws:
	cd integration && go test -c -o integration.test . && INTEGRATION_BYOH_AWS=1 ./integration.test -test.v -test.timeout 65m -test.run '^TestBYOHOnAWS$$'

# Creates and deletes VMs on your own Proxmox VE server (free). Setup and variables:
# docs/testing/proxmox.md. Cluster file: TOW_TEST_PROXMOX_CONFIG (default
# examples/dev-test-proxmox.yaml, two VMs of 2 GB; examples/homelab-proxmox-ha.yaml for three
# control planes behind kube-vip and a failover). The timeout leaves room for the test's 35 min
# deadline plus cleanup.
test-proxmox:
	INTEGRATION_PROXMOX=1 go test -v -count=1 -timeout 50m -run '^TestProxmoxInfra$$' ./integration/...

# One cluster's whole life on Proxmox (DIST=k3s or kubeadm, about an hour, unattended):
# machines, add-ons, firewall, settings, a backup, an upgrade with Postgres written to
# throughout, a restore, certificates renewed.
test-lifecycle:
	INTEGRATION_PROXMOX=1 TOW_TEST_LIFECYCLE=$(DIST) go test -v -count=1 -timeout 170m -run '^TestProxmoxLifecycle$$' ./integration/...

# Opt-in: the kubeadm lifecycle with two workers, then Kubernetes' conformance tests (Sonobuoy,
# certified-conformance) on the cluster it upgraded and restored: about 3 to 4 hours,
# unattended. The results stay in integration/conformance/.
test-lifecycle-conformance:
	cd integration && go test -c -o integration.test . && INTEGRATION_PROXMOX=1 TOW_TEST_LIFECYCLE=kubeadm TOW_TEST_CONFORMANCE=1 \
		./integration.test -test.v -test.timeout 6h -test.run '^TestProxmoxLifecycle$$'

# A kubeadm cluster's life on AWS, on two t3.small (about 45 minutes, ~$0.04): a worker added in
# another zone (the others untouched) and removed, new settings, the upgrade with Postgres
# written to, certificates renewed. Deleted even after a failure (TOW_TEST_LIFECYCLE_KEEP=1
# keeps it); exit code 3 means AWS may still have its machines (the output names them).
# TOW_TEST_CONFORMANCE=1: two workers, and the conformance tests after the upgrade (about
# 1.5 hours more, ~$0.15). The timeout leaves room for the test's own deadline (75 min, 3 hours
# more with the conformance tests) plus cleanup; the same on GCP and Azure.
test-aws-lifecycle:
	cd integration && go test -c -o integration.test . && INTEGRATION_AWS_LIFECYCLE=1 ./integration.test -test.v -test.timeout 5h -test.run '^TestAWSLifecycle$$'

# The GCP tests run like the AWS ones (exit code 3: machines may still be up), in the project
# TOW_TEST_GCP_PROJECT names (Compute Engine and IAM APIs on, your Application Default
# Credentials), region TOW_TEST_GCP_REGION (default europe-west1); docs/testing/gcp.md.
# They stay within a free trial's 4 public addresses.

# Builds a kubeadm cluster on two real e2-medium machines, checks it, deletes it (~$0.02 a run).
# TOW_TEST_OS: almalinux-9 or almalinux-10 instead of Ubuntu; TOW_TEST_KUBERNETES as on
# AWS; a cluster file instead: TOW_TEST_GCP_CONFIG (../examples/pd-gcp.yaml: persistent
# disks).
test-gcp:
	cd integration && go test -c -o integration.test . && INTEGRATION_GCP=1 ./integration.test -test.v -test.timeout 70m -test.run '^TestGCPInfra$$'

# Three control planes, one per zone, behind the two passthrough load balancers; the first
# is stopped and must not be missed (~$0.06 a run). k3s instead of kubeadm:
# TOW_TEST_KUBERNETES='{distribution: k3s}'.
test-gcp-ha:
	cd integration && go test -c -o integration.test . && INTEGRATION_GCP_HA=1 ./integration.test -test.v -test.timeout 70m -test.run '^TestGCPHA$$'

# test-aws-lifecycle's stages on GCP (two e2-medium, three while a worker is added; ~$0.05).
test-gcp-lifecycle:
	cd integration && go test -c -o integration.test . && INTEGRATION_GCP_LIFECYCLE=1 ./integration.test -test.v -test.timeout 5h -test.run '^TestGCPLifecycle$$'

# The Azure tests run like the AWS ones (exit code 3: machines may still be up), in the
# subscription TOW_TEST_AZURE_SUBSCRIPTION names (your az login, or a service principal),
# location TOW_TEST_AZURE_LOCATION (default swedencentral); docs/testing/azure.md.

# Builds a kubeadm cluster on two real Standard_B2s_v2 VMs, checks it, deletes it (~$0.03 a run).
# TOW_TEST_OS: almalinux-9 or almalinux-10; TOW_TEST_KUBERNETES as on AWS; a cluster
# file instead: TOW_TEST_AZURE_CONFIG (../examples/disk-azure.yaml: managed disks).
test-azure:
	cd integration && go test -c -o integration.test . && INTEGRATION_AZURE=1 ./integration.test -test.v -test.timeout 70m -test.run '^TestAzureInfra$$'

# Three control planes, one per zone, behind the two Standard load balancers; the first is
# deallocated and must not be missed (~$0.08 a run).
test-azure-ha:
	cd integration && go test -c -o integration.test . && INTEGRATION_AZURE_HA=1 ./integration.test -test.v -test.timeout 70m -test.run '^TestAzureHA$$'

# test-aws-lifecycle's stages on Azure (two Standard_B2s_v2, three while a worker is added; ~$0.07).
test-azure-lifecycle:
	cd integration && go test -c -o integration.test . && INTEGRATION_AZURE_LIFECYCLE=1 ./integration.test -test.v -test.timeout 5h -test.run '^TestAzureLifecycle$$'

# Every line of integration/proxmox-matrix.txt, one at a time (3 to 4 hours, unattended): a
# summary at the end, the logs in integration/logs/. ONLY=<text> runs only the lines that
# contain it, to run again what failed.
test-proxmox-all:
	integration/matrix.sh integration/proxmox-matrix.txt "$(ONLY)"

# Opt-in: Kubernetes' conformance tests (Sonobuoy, in the certified-conformance mode CNCF's
# certification uses) on a lab cluster, about 2 hours. Default cluster: kubeadm with two
# workers (examples/custom-proxmox.yaml). The results stay in integration/conformance/.
test-conformance:
	cd integration && go test -c -o integration.test . && INTEGRATION_PROXMOX=1 TOW_TEST_CONFORMANCE=1 \
		TOW_TEST_PROXMOX_CONFIG=$${TOW_TEST_PROXMOX_CONFIG:-../examples/custom-proxmox.yaml} \
		./integration.test -test.v -test.timeout 5h -test.run '^TestProxmoxInfra$$'

build:
	go build -o tow

clean:
	rm -f tow

# Scan staged changes for secrets before every commit.
hooks:
	git config core.hooksPath .githooks

# Scan the whole git history for secrets, like CI does.
secrets:
	gitleaks git --redact --verbose .

# The linters CI runs (.golangci.yml); needs golangci-lint v2 (https://golangci-lint.run).
lint:
	golangci-lint run ./...

# Unit-test coverage, as CI measures it for Codecov: the total, and an HTML page per file.
cover:
	go test -short -coverprofile=coverage.out ./internal/... ./cmd/...
	go tool cover -func=coverage.out | tail -1
	go tool cover -html=coverage.out -o coverage.html
	@echo "Open coverage.html to see which lines the tests run."