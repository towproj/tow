package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/towproj/tow/internal/ui"
)

// fakeHelm writes a helm that reports release v.
func fakeHelm(t *testing.T, dir, v string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "helm")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho "+v+"+gabc1234\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// world is a test machine: a PATH folder, ~/.local/bin, and a download server whose archive
// holds a helm reporting the pinned release.
type world struct {
	path, bin string
	tool      Tool
	asked     []string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{path: t.TempDir(), bin: filepath.Join(t.TempDir(), ".local", "bin")}
	t.Setenv("PATH", w.path)
	origBin, origPlatform := binDir, platform
	binDir, platform = func() (string, error) { return w.bin, nil }, "linux-amd64"
	t.Cleanup(func() { binDir, platform = origBin, origPlatform })

	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	program := []byte("#!/bin/sh\necho v4.3.1+gtest\n")
	tw.WriteHeader(&tar.Header{Name: "linux-amd64/helm", Mode: 0o755, Size: int64(len(program)), Typeflag: tar.TypeReg})
	tw.Write(program)
	tw.Close()
	gz.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { rw.Write(archive.Bytes()) }))
	t.Cleanup(srv.Close)

	sum := sha256.Sum256(archive.Bytes())
	w.tool = Helm
	w.tool.url = func(release, platform string) string {
		return srv.URL + "/helm-" + release + "-" + platform + ".tar.gz"
	}
	w.tool.sums = map[string]string{"linux-amd64": hex.EncodeToString(sum[:])}
	return w
}

func (w *world) options(answer bool) Options {
	return Options{Ask: func(q string) bool { w.asked = append(w.asked, q); return answer }}
}

// A connection dropped without an answer (a DNS or network hiccup) is tried again; an
// answer with an error status is final.
func TestGetRetriesNetworkErrors(t *testing.T) {
	defer func(d time.Duration) { getRetryEvery = d }(getRetryEvery)
	getRetryEvery = time.Millisecond
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Path {
		case "/flaky":
			if calls == 1 {
				conn, _, _ := rw.(http.Hijacker).Hijack()
				conn.Close() // no answer at all
				return
			}
			rw.Write([]byte("v1.37.2"))
		default:
			http.NotFound(rw, r)
		}
	}))
	defer srv.Close()

	got, err := fetch(context.Background(), srv.URL+"/flaky")
	if err != nil || got != "v1.37.2" || calls != 2 {
		t.Errorf("fetch = %q, %v after %d calls", got, err, calls)
	}
	calls = 0
	if _, err := fetch(context.Background(), srv.URL+"/missing"); err == nil || !strings.Contains(err.Error(), "404") || calls != 1 {
		t.Errorf("a 404 is final: %v after %d calls", err, calls)
	}
}

func TestFindUsesASupportedRelease(t *testing.T) {
	for _, v := range []string{"v3.8.0", "v3.22.1", "v4.3.1", "v4.9.0"} {
		w := newWorld(t)
		want := fakeHelm(t, w.path, v)
		got, err := Find(context.Background(), w.tool, w.options(false))
		if err != nil || got != want || len(w.asked) != 0 {
			t.Errorf("%s: path %q, err %v, asked %v", v, got, err, w.asked)
		}
	}
}

// A helm too old (or too new) is left alone; the tested one goes to ~/.local/bin after a yes.
func TestUnsupportedReleaseInstallsAfterAYes(t *testing.T) {
	for _, v := range []string{"v3.7.2", "v5.0.0"} {
		w := newWorld(t)
		old := fakeHelm(t, w.path, v)
		got, err := Find(context.Background(), w.tool, w.options(true))
		if err != nil {
			t.Fatal(err)
		}
		if got != filepath.Join(w.bin, "helm") || len(w.asked) != 1 || !strings.Contains(w.asked[0], "Install helm v4.3.1 into "+w.bin) {
			t.Errorf("%s: path %q, asked %v", v, got, w.asked)
		}
		info, err := os.Stat(got)
		if err != nil || info.Mode().Perm() != 0o755 {
			t.Errorf("installed helm: %v, %v", info, err)
		}
		if data, _ := os.ReadFile(old); !strings.Contains(string(data), v) {
			t.Error("the user's own helm must stay as it was")
		}
	}
}

func TestMissingHelm(t *testing.T) {
	ctx := context.Background()

	// A no installs nothing.
	w := newWorld(t)
	_, err := Find(ctx, w.tool, w.options(false))
	if err == nil || !strings.Contains(err.Error(), "nothing was done") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.bin, "helm")); err == nil {
		t.Error("installed despite a no")
	}

	// Nobody to ask: stop, naming the flag.
	ui.SetNonInteractive(true)
	t.Cleanup(func() { ui.SetNonInteractive(false) })
	w = newWorld(t)
	_, err = Find(ctx, w.tool, Options{})
	if err == nil || !strings.Contains(err.Error(), "--install-tools") {
		t.Errorf("err = %v", err)
	}

	// --install-tools: installed without a question.
	w = newWorld(t)
	got, err := Find(ctx, w.tool, Options{Install: true})
	if err != nil || got != filepath.Join(w.bin, "helm") {
		t.Errorf("path %q, err %v", got, err)
	}

	// A dry run says what would happen, and installs nothing.
	w = newWorld(t)
	if got, err := Find(ctx, w.tool, Options{DryRun: true}); err != nil || got != "helm" {
		t.Errorf("dry run: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(w.bin, "helm")); err == nil {
		t.Error("a dry run installed helm")
	}
}

// Replacing the user's own copy in ~/.local/bin needs a yes that names both releases.
func TestReplacingNamesBothReleases(t *testing.T) {
	w := newWorld(t)
	fakeHelm(t, w.bin, "v3.5.0")
	t.Setenv("PATH", w.bin)
	if _, err := Find(context.Background(), w.tool, w.options(true)); err != nil {
		t.Fatal(err)
	}
	if len(w.asked) != 1 || !strings.Contains(w.asked[0], "Replace helm 3.5.0 with v4.3.1") {
		t.Errorf("asked %v", w.asked)
	}

	// A copy that doesn't say its version is named as such, not as release 0.0.0.
	w = newWorld(t)
	if err := os.WriteFile(fakeHelm(t, w.bin, "x"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", w.bin)
	if _, err := Find(context.Background(), w.tool, w.options(true)); err != nil {
		t.Fatal(err)
	}
	if len(w.asked) != 1 || !strings.Contains(w.asked[0], "Replace helm (it doesn't report its version) with v4.3.1") {
		t.Errorf("asked %v", w.asked)
	}
}

// A good helm in ~/.local/bin is used even when that folder isn't on PATH.
func TestMineOffPath(t *testing.T) {
	w := newWorld(t)
	want := fakeHelm(t, w.bin, "v4.3.1")
	if got, err := Find(context.Background(), w.tool, w.options(false)); err != nil || got != want {
		t.Errorf("path %q, err %v", got, err)
	}
}

func TestChecksumMismatchInstallsNothing(t *testing.T) {
	w := newWorld(t)
	w.tool.sums = map[string]string{"linux-amd64": strings.Repeat("0", 64)}
	_, err := Find(context.Background(), w.tool, w.options(true))
	if err == nil || !strings.Contains(err.Error(), "doesn't match the sha256") {
		t.Errorf("err = %v", err)
	}
	if entries, _ := os.ReadDir(w.bin); len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
}

func TestUnknownPlatform(t *testing.T) {
	w := newWorld(t)
	platform = "plan9-386"
	_, err := Find(context.Background(), w.tool, w.options(true))
	if err == nil || !strings.Contains(err.Error(), "no tested helm build for plan9-386") || len(w.asked) != 0 {
		t.Errorf("err = %v, asked %v", err, w.asked)
	}
}

// The real downloads, into throwaway folders (INTEGRATION=1): helm from get.helm.sh, tofu
// and kubectl.
func TestRealDownloads(t *testing.T) {
	if os.Getenv("INTEGRATION") == "" {
		t.Skip("set INTEGRATION=1 to download helm, tofu and kubectl for real")
	}
	path, err := Download(context.Background(), Helm, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if v, err := versionOf(context.Background(), path, Helm.versionArgs); err != nil || "v"+v.String() != Helm.Pinned {
		t.Errorf("downloaded helm reports %v, %v", v, err)
	}
	// tofu, from OpenTofu's GitHub release (a redirect to its storage), out of its tarball.
	path, err = Download(context.Background(), Tofu, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if v, err := versionOf(context.Background(), path, Tofu.versionArgs); err != nil || "v"+v.String() != Tofu.Pinned {
		t.Errorf("downloaded tofu reports %v, %v", v, err)
	}
	// kubectl's newest patch of two minor releases, checked against the sum dl.k8s.io
	// publishes.
	for _, cluster := range []string{"v1.37.2", "v1.34.0+k3s1"} {
		k := Kubectl(cluster)
		path, release, err := install(context.Background(), k, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if v, err := versionOf(context.Background(), path, k.versionArgs); err != nil || "v"+v.String() != release || !k.supports(v) {
			t.Errorf("kubectl for %s: downloaded %s, reports %v, %v", cluster, release, v, err)
		}
		t.Logf("kubectl for %s: %s", cluster, release)
	}
}

// tofu: 1.12 and newer within 1.x (the releases OpenTofu patches); the pinned release is one of
// them, with a sum for every platform tow builds for.
func TestTofu(t *testing.T) {
	for v, want := range map[string]bool{"1.11.14": false, "1.12.0": true, "1.13.9": true, "1.20.0": true, "2.0.0": false} {
		pv, _ := parseVersion(v)
		if Tofu.supports(pv) != want {
			t.Errorf("tofu %s: %v", v, !want)
		}
	}
	if pv, _ := parseVersion(Tofu.Pinned); !Tofu.supports(pv) {
		t.Errorf("pinned %s isn't supported", Tofu.Pinned)
	}
	for _, p := range []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64"} {
		if !sha256Hex.MatchString(Tofu.sums[p]) {
			t.Errorf("no sum for %s", p)
		}
	}
	if got := Tofu.url("v1.13.9", "linux-amd64"); got != "https://github.com/opentofu/opentofu/releases/download/v1.13.9/tofu_1.13.9_linux_amd64.tar.gz" {
		t.Errorf("url = %s", got)
	}
	// 'tofu version' prints "OpenTofu v1.13.9" then the platform.
	if v, err := parseVersion("OpenTofu v1.13.9\non linux_amd64\n"); err != nil || v.String() != "1.13.9" {
		t.Errorf("version = %v, %v", v, err)
	}
}

// kubectl must be the cluster's minor release, any patch; tow installs nothing pinned
// but the newest patch dl.k8s.io names for that minor.
func TestKubectl(t *testing.T) {
	k := Kubectl("v1.37.0+k3s1")
	for v, want := range map[string]bool{"1.36.9": false, "1.37.0": true, "1.37.12": true, "1.38.0": false} {
		pv, _ := parseVersion(v)
		if k.supports(pv) != want {
			t.Errorf("kubectl %s with a 1.37 cluster: %v", v, !want)
		}
	}
	if k.Pinned != "" || k.sums != nil || k.latestURL != "https://dl.k8s.io/release/stable-1.37.txt" || k.Supports != "1.37 (the cluster's minor release, any patch)" {
		t.Errorf("kubectl for a 1.37 cluster: %+v", k)
	}
	if got := k.url("v1.37.4", "linux-amd64"); got != "https://dl.k8s.io/release/v1.37.4/bin/linux/amd64/kubectl" {
		t.Errorf("url = %s", got)
	}
}

// The newest patch of the cluster's minor, named by dl.k8s.io and checked against the sha256
// published beside it; a kubectl of another minor isn't used, one of another patch is.
func TestKubectlNewestPatch(t *testing.T) {
	newest := "v1.34.5\n"
	program := []byte("#!/bin/sh\necho 'Client Version: v1.34.5'\n")
	sum := sha256.Sum256(program)
	published := hex.EncodeToString(sum[:])
	var served []string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		served = append(served, r.URL.Path)
		switch r.URL.Path {
		case "/release/stable-1.34.txt":
			rw.Write([]byte(newest))
		case "/release/v1.34.5/bin/linux/amd64/kubectl":
			rw.Write(program)
		case "/release/v1.34.5/bin/linux/amd64/kubectl.sha256":
			rw.Write([]byte(published))
		default:
			http.NotFound(rw, r)
		}
	}))
	defer srv.Close()
	orig := kubectlDownloads
	kubectlDownloads = srv.URL
	t.Cleanup(func() { kubectlDownloads = orig })
	k := Kubectl("v1.34.3+k3s1")
	fake := func(dir, release string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte("#!/bin/sh\necho 'Client Version: "+release+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Another patch of the cluster's minor is used as it is.
	w := newWorld(t)
	fake(w.path, "v1.34.0")
	if path, err := Find(context.Background(), k, w.options(true)); err != nil || path != filepath.Join(w.path, "kubectl") || len(w.asked) != 0 {
		t.Errorf("1.34.0 on PATH: %q, %v, asked %v", path, err, w.asked)
	}

	// Another minor isn't: a dry run says what would be installed, and fetches nothing.
	w = newWorld(t)
	fake(w.path, "v1.35.1")
	if _, err := Find(context.Background(), k, Options{DryRun: true}); err != nil || len(served) != 0 {
		t.Errorf("dry run: %v, served %v", err, served)
	}
	path, err := Find(context.Background(), k, w.options(true))
	if err != nil || path != filepath.Join(w.bin, "kubectl") {
		t.Fatalf("path %q, err %v", path, err)
	}
	if len(w.asked) != 1 || !strings.Contains(w.asked[0], "Install kubectl 1.34 (its newest patch) into") {
		t.Errorf("asked %v", w.asked)
	}

	// dl.k8s.io names a release of another minor: nothing is installed.
	w = newWorld(t)
	newest = "v1.35.0"
	if _, err := Find(context.Background(), k, w.options(true)); err == nil || !strings.Contains(err.Error(), `names "v1.35.0", not a kubectl 1.34`) {
		t.Errorf("err = %v", err)
	}
	newest = "v1.34.5"

	// The published sum doesn't match: nothing is installed.
	w = newWorld(t)
	published = strings.Repeat("0", 64) + "  kubectl"
	if _, err := Find(context.Background(), k, w.options(true)); err == nil || !strings.Contains(err.Error(), "doesn't match the sha256") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.bin, "kubectl")); err == nil {
		t.Error("a kubectl that didn't match was installed")
	}

	// The sum file holds no sum: nothing is installed either.
	w = newWorld(t)
	published = "<html>not found</html>"
	if _, err := Find(context.Background(), k, w.options(true)); err == nil || !strings.Contains(err.Error(), "doesn't hold a sha256") {
		t.Errorf("err = %v", err)
	}
}
