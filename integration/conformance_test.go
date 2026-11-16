package integration

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/state"
)

// Sonobuoy runs Kubernetes' conformance tests the way CNCF's certification (Certified
// Kubernetes) asks for. Pinned, and checked against the sum from its release's checksums file.
const (
	sonobuoyVersion = "0.57.5"
	sonobuoySum     = "7c5c2250e5103c98f4dad2bcab86baf954e319c57c419c113f28f5060e62f129" // linux_amd64
)

// conformance reports whether this run asks for the conformance tests (make test-conformance).
func conformance() bool { return os.Getenv("TOW_TEST_CONFORMANCE") != "" }

// checkConformance runs the conformance tests on the cluster in certified-conformance mode,
// fails on any failed test, and keeps Sonobuoy's results in integration/conformance/<cluster>/:
// the files a submission to CNCF's k8s-conformance repository is made from.
func checkConformance(t *testing.T, ctx context.Context, st *state.ClusterState) {
	t.Helper()
	bin := sonobuoy(t, ctx)
	run := func(args ...string) string {
		t.Helper()
		if args[0] != "results" { // results only reads the archive, and refuses --kubeconfig
			args = append(args, "--kubeconfig", st.Outputs.Kubeconfig)
		}
		out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("sonobuoy %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	t.Logf("conformance tests (Sonobuoy %s, certified-conformance), usually 1.5 to 2 hours", sonobuoyVersion)
	run("run", "--mode=certified-conformance", "--wait=240", "--wait-output=silent") // minutes
	dir, err := filepath.Abs(filepath.Join("conformance", st.Name+"-"+st.K8sVersion))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(run("retrieve", dir), "\n")
	tarball := lines[len(lines)-1]
	results := run("results", tarball)
	if err := os.WriteFile(filepath.Join(dir, "results.txt"), []byte(results+"\n"), 0o644); err != nil {
		t.Error(err)
	}
	t.Logf("conformance results, kept in %s:\n%s", dir, results)
	if !strings.Contains(results, "Status: passed") {
		t.Errorf("conformance tests didn't all pass: see %s and %s", tarball, filepath.Join(dir, "results.txt"))
	}
	run("delete", "--wait", "--wait-output=silent")
}

// sonobuoy downloads the pinned Sonobuoy into a throwaway folder, after checking its sum.
func sonobuoy(t *testing.T, ctx context.Context) string {
	t.Helper()
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Fatalf("the pinned Sonobuoy is for linux/amd64, not %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	url := fmt.Sprintf("https://github.com/vmware-tanzu/sonobuoy/releases/download/v%s/sonobuoy_%s_linux_amd64.tar.gz", sonobuoyVersion, sonobuoyVersion)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("download Sonobuoy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download Sonobuoy: %s", resp.Status)
	}
	archive, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("download Sonobuoy: %v", err)
	}
	if sum := sha256.Sum256(archive); hex.EncodeToString(sum[:]) != sonobuoySum {
		t.Fatalf("Sonobuoy's archive has sha256 %x, want %s", sum, sonobuoySum)
	}
	gz, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatalf("no sonobuoy binary in its archive: %v", err)
		}
		if filepath.Base(h.Name) != "sonobuoy" || h.Typeflag != tar.TypeReg {
			continue
		}
		path := filepath.Join(t.TempDir(), "sonobuoy")
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
}
