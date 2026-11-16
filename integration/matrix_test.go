package integration

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/config"
)

// Every line of the Proxmox matrix is a valid cluster file for Proxmox, so a typo fails here,
// in seconds, not hours into an unattended run.
func TestProxmoxMatrixParses(t *testing.T) {
	f, err := os.Open("proxmox-matrix.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	for s := bufio.NewScanner(f); s.Scan(); {
		line := s.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		n++
		file, extra, _ := strings.Cut(line, " ")
		// "lifecycle:<distribution>": TestProxmoxLifecycle, whose own file must parse.
		if dist, ok := strings.CutPrefix(file, "lifecycle:"); ok {
			if dist != config.DistributionK3s && dist != config.DistributionKubeadm {
				t.Errorf("%s: a lifecycle line names k3s or kubeadm", line)
				continue
			}
			if _, err := config.ParseIn([]byte(lifecycleFile(lifecycleRun{provider: config.ProviderProxmox, dist: dist}, "test-life", "v1.36.6")), "."); err != nil {
				t.Errorf("%s: %v", line, err)
			}
			continue
		}
		path := filepath.Join("..", "examples", file)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v", line, err)
			continue
		}
		if extra != "" {
			data = withKubernetes(t, data, extra)
		}
		cfg, err := config.ParseIn(data, filepath.Dir(path))
		if err != nil {
			t.Errorf("%s: %v", line, err)
			continue
		}
		if cfg.Provider() != config.ProviderProxmox {
			t.Errorf("%s: provider %s, want proxmox", line, cfg.Provider())
		}
	}
	if n < 20 {
		t.Errorf("%d lines; the matrix covers more", n)
	}
}

// The lifecycle runs' other files parse too (AWS; the lab's kubeadm with two workers for the
// conformance tests), and hold the lines their stages edit.
func TestLifecycleFilesParse(t *testing.T) {
	for _, c := range []struct {
		name        string
		run         lifecycleRun
		conformance bool
		provider    string
	}{
		{"aws", lifecycleRun{provider: config.ProviderAWS, dist: config.DistributionKubeadm, region: "eu-west-3"}, false, config.ProviderAWS},
		{"gcp", lifecycleRun{provider: config.ProviderGCP, dist: config.DistributionKubeadm, region: "europe-west1", project: "demo-project"}, false, config.ProviderGCP},
		{"azure", lifecycleRun{provider: config.ProviderAzure, dist: config.DistributionKubeadm, region: "westeurope", subscription: "00000000-0000-0000-0000-000000000000"}, false, config.ProviderAzure},
		{"conformance", lifecycleRun{provider: config.ProviderProxmox, dist: config.DistributionKubeadm}, true, config.ProviderProxmox},
	} {
		t.Run(c.name, func(t *testing.T) {
			conf := ""
			if c.conformance {
				conf = "1"
			}
			t.Setenv("TOW_TEST_CONFORMANCE", conf)
			text := lifecycleFile(c.run, "test-life", "v1.36.6")
			cfg, err := config.ParseIn([]byte(text), ".")
			if err != nil {
				t.Fatalf("%v\n%s", err, text)
			}
			if cfg.Provider() != c.provider || cfg.Distribution() != config.DistributionKubeadm || cfg.MachineCount() != 1+lifecycleWorkers() {
				t.Errorf("provider %s, %s, %d machines:\n%s", cfg.Provider(), cfg.Distribution(), cfg.MachineCount(), text)
			}
			for _, line := range []string{workersLine(lifecycleWorkers()), "version: v1.36.6", "    kubeconfig: {merge: false}"} {
				if !strings.Contains(text, line) {
					t.Errorf("no %q in:\n%s", line, text)
				}
			}
		})
	}
}
