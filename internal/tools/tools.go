// Package tools finds the command-line tools tow runs on this machine (helm, kubectl,
// tofu) and, only after the user said yes, installs one into ~/.local/bin, where it stays for
// the user: helm and tofu, the release tow was tested with; kubectl, the newest patch of
// the cluster's minor release. A tool already installed is never replaced or hidden without a yes that
// names both versions.
package tools

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/towproj/tow/internal/ui"
)

// Tool is one program tow runs, the releases it works with, and the one it installs.
type Tool struct {
	Name     string
	Supports string // the releases that work, in words
	// Pinned is the release tow installs, tested; "" = the newest one latestURL names,
	// read only once the user said yes (kubectl: the newest patch of the cluster's minor).
	Pinned    string
	latestURL string
	want      string // what tow installs, in words, when nothing is pinned
	supports  func(v version) bool
	url       func(release, platform string) string
	// sums are the sha256 of each pinned archive, by platform (linux-amd64, ...). They're
	// part of tow, checked against two sources when the release was pinned, so a
	// download is never trusted on its own server's word.
	sums map[string]string
	// sumURL is where the project publishes a release's sha256, for a release tow
	// carries no sum for (kubectl of the cluster's own release); nil = only the pinned sums.
	sumURL func(release, platform string) string
	// offer says which release tow installs, for the plan it shows.
	offer  string
	member func(platform string) string // the program's path inside the archive; nil = the download is the program
	// versionArgs make the tool print its release.
	versionArgs []string
}

// Helm releases: 3.8 brought OCI charts; every flag tow uses (upgrade --install, --repo,
// --create-namespace, --wait, --set-file) works the same in 3.x and 4.x (checked on v3.22.1
// and v4.3.1). 4.x applies server-side, so it stops on a field a controller took over where
// 3.x overwrote it; --force-conflicts (4.x only) takes it back.
var Helm = Tool{
	Name:     "helm",
	Supports: "3.8 or newer, or 4.x",
	Pinned:   "v4.3.1",
	supports: func(v version) bool { return (v.major == 3 && v.minor >= 8) || v.major == 4 },
	url: func(release, platform string) string {
		return helmDownloads + "/helm-" + release + "-" + platform + ".tar.gz"
	},
	// get.helm.sh/*.sha256sum and the v4.3.1 release notes on GitHub, 2026-11-09.
	sums: map[string]string{
		"linux-amd64":  "0403a88ae20913d11b10a2467b50087721fea7581736da6b9d3992f5357fe54b",
		"linux-arm64":  "8cf8f66ecbc252ac8f7f06cca68aba88508ddfde3a6a34feb1678ad1694b4f88",
		"darwin-amd64": "34bec4eba761ba5c16d1965ee6b8f5b0abe5993974a802274e2a4f0418893d89",
		"darwin-arm64": "6b4e61c8cd27d99e5a801e63ff25dd5c70b36875c2cd659d3e9ce6b76fc23198",
	},
	member:      func(platform string) string { return platform + "/helm" },
	versionArgs: []string{"version", "--short"},
}

// Tofu is OpenTofu, which creates the machines on a cloud or a Proxmox server. 1.12 and 1.13
// are the releases OpenTofu still patches; v1.13.9 is the one tested.
var Tofu = Tool{
	Name:     "tofu",
	Supports: "1.12 or a newer 1.x",
	Pinned:   "v1.13.9",
	supports: func(v version) bool { return v.major == 1 && v.minor >= 12 },
	url: func(release, platform string) string {
		v := strings.TrimPrefix(release, "v")
		return tofuDownloads + "/" + release + "/tofu_" + v + "_" + strings.Replace(platform, "-", "_", 1) + ".tar.gz"
	},
	// tofu_1.13.9_SHA256SUMS from the GitHub release, its GPG signature checked with
	// OpenTofu's key (get.opentofu.org/opentofu.asc, E3E6 E43D 84CB 852E ADB0 051D 0C0A F313
	// E5FD 9F80), 2026-11-14.
	sums: map[string]string{
		"linux-amd64":  "b34b327cc1ef07178207b6f2cd1f5276537d3e5b7be316500f6dac50d4c0a857",
		"linux-arm64":  "5dc196dde75b667e4806d358310ec96742774cd0993c54c1237efee5644b69b8",
		"darwin-amd64": "347e2e5a2cfe2f368829572d308b2e7c1e0486b20a1779090ff06360f7551e4f",
		"darwin-arm64": "496f4c3064b5c3bff79174b3dae4faaf65a66774eb8e2bf074f895412c5f124f",
	},
	member:      func(string) string { return "tofu" },
	versionArgs: []string{"version"},
}

// Kubectl is kubectl for a cluster of the given Kubernetes release (v1.37.2, or k3s's
// v1.37.2+k3s1): the cluster's minor release, any patch. tow installs the newest patch of
// that minor, the one dl.k8s.io/release/stable-1.<minor>.txt names, checked against the
// sha256 dl.k8s.io publishes beside it.
func Kubectl(cluster string) Tool {
	cv, _ := parseVersion(cluster)
	minor := cv.minor
	url := func(release, platform string) string {
		return kubectlDownloads + "/release/" + release + "/bin/" + strings.Replace(platform, "-", "/", 1) + "/kubectl"
	}
	return Tool{
		Name:        "kubectl",
		Supports:    fmt.Sprintf("1.%d (the cluster's minor release, any patch)", minor),
		supports:    func(v version) bool { return v.major == 1 && v.minor == minor },
		latestURL:   fmt.Sprintf("%s/release/stable-1.%d.txt", kubectlDownloads, minor),
		want:        fmt.Sprintf("1.%d (its newest patch)", minor),
		offer:       fmt.Sprintf("the newest kubectl 1.%d", minor),
		url:         url,
		sumURL:      func(release, platform string) string { return url(release, platform) + ".sha256" },
		versionArgs: []string{"version", "--client"},
	}
}

// Vars so tests use a local server and folder.
var (
	helmDownloads    = "https://get.helm.sh"
	kubectlDownloads = "https://dl.k8s.io"
	tofuDownloads    = "https://github.com/opentofu/opentofu/releases/download"
	binDir           = func() (string, error) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "bin"), nil
	}
	platform = runtime.GOOS + "-" + runtime.GOARCH
)

// Options say what may happen when a tool is missing or too old.
type Options struct {
	// Install installs without asking (--install-tools): for runs nobody watches.
	Install bool
	// DryRun says what would happen, and installs nothing.
	DryRun bool
	// Ask asks a yes/no question; nil = ui.Confirm when someone can answer.
	Ask func(question string) bool
}

// Installed is the path of a release of the tool that works, on PATH or in ~/.local/bin; ""
// when there's none. It never asks or installs: for commands that only read.
func Installed(ctx context.Context, t Tool) string {
	dir, err := binDir()
	if err != nil {
		return ""
	}
	for _, path := range candidates(t.Name, filepath.Join(dir, t.Name)) {
		if v, err := versionOf(ctx, path, t.versionArgs); err == nil && t.supports(v) {
			return path
		}
	}
	return ""
}

// Major is the major release of the tool at path (4 for helm v4.3.1); 0 when it can't tell.
func Major(ctx context.Context, t Tool, path string) int {
	v, err := versionOf(ctx, path, t.versionArgs)
	if err != nil {
		return 0
	}
	return v.major
}

// Find returns the path of a release of the tool that works: on PATH, or in ~/.local/bin.
// Otherwise it explains, asks, and installs the pinned release. A dry run never installs.
func Find(ctx context.Context, t Tool, o Options) (string, error) {
	dir, err := binDir()
	if err != nil {
		return "", err
	}
	mine := filepath.Join(dir, t.Name)

	var found []string // tools that don't fit, described
	var mineFound string
	for _, path := range candidates(t.Name, mine) {
		v, err := versionOf(ctx, path, t.versionArgs)
		switch {
		case err != nil:
			found = append(found, fmt.Sprintf("%s doesn't report its version (%v)", path, err))
		case t.supports(v):
			if path == mine && !onPath(dir) {
				ui.Hint(fmt.Sprintf("%s isn't on your PATH: add it to use %s yourself (export PATH=\"%s:$PATH\" in ~/.profile)", dir, t.Name, dir))
			}
			ui.Debug("%s %s at %s", t.Name, v, path)
			return path, nil
		default:
			found = append(found, fmt.Sprintf("%s at %s is %s; tow works with %s", t.Name, path, v, t.Supports))
		}
		if path == mine {
			mineFound = v.String()
			if err != nil {
				mineFound = "(it doesn't report its version)"
			}
		}
	}

	what := fmt.Sprintf("%s isn't installed", t.Name)
	if len(found) > 0 {
		what = strings.Join(found, "; ")
	}
	_, builtIn := t.sums[platform]
	if !builtIn && t.sumURL == nil {
		return "", fmt.Errorf("%s; tow has no tested %s build for %s: install %s %s yourself", what, t.Name, platform, t.Name, t.Supports)
	}
	if pinned, _ := parseVersion(t.Pinned); t.Pinned != "" && !t.supports(pinned) {
		return "", fmt.Errorf("%s; the %s tow installs (%s) doesn't fit either: install %s %s yourself", what, t.Name, t.Pinned, t.Name, t.Supports)
	}
	offer, checked := cmp.Or(t.offer, "the release it was tested with"), "sha256 checked"
	if !builtIn {
		checked = "sha256 checked against the one published beside it"
	}
	from := fmt.Sprintf("  %s %s from %s (%s)", t.Name, t.Pinned, t.url(t.Pinned, platform), checked)
	if t.Pinned == "" {
		from = fmt.Sprintf("  %s %s, the release %s names, from %s (%s)", t.Name, t.want, t.latestURL, t.url("<that release>", platform), checked)
	}
	plan := []string{fmt.Sprintf("%s. tow can install %s:", what, offer), from, fmt.Sprintf("  → %s", mine)}
	if mineFound != "" {
		plan = append(plan, fmt.Sprintf("  this replaces your %s %s there", mine, mineFound))
	}
	if other, err := exec.LookPath(t.Name); err == nil && other != mine {
		plan = append(plan, fmt.Sprintf("  your %s stays as it is; %s", other, firstOnPath(dir, other)))
	}
	if o.DryRun {
		for _, line := range plan {
			ui.DryRun(line)
		}
		ui.DryRun("You'd be asked before anything is installed ('--install-tools' installs without asking)")
		return t.Name, nil
	}

	for _, line := range plan {
		fmt.Println(ui.Redact(line))
	}
	if !o.Install {
		ask := o.Ask
		if ask == nil {
			if !ui.Interactive() {
				return "", fmt.Errorf("%s, and nobody can answer here: install %s %s yourself, or run with --install-tools", what, t.Name, t.Supports)
			}
			ask = ui.Confirm
		}
		question := fmt.Sprintf("Install %s %s into %s?", t.Name, t.shown(), dir)
		if mineFound != "" {
			question = fmt.Sprintf("Replace %s %s with %s in %s?", t.Name, mineFound, t.shown(), dir)
		}
		if !ask(question) {
			return "", fmt.Errorf("%s isn't installed, so nothing was done: install %s %s yourself, then run tow again", t.Name, t.Name, t.Supports)
		}
	}

	done := ui.Step(fmt.Sprintf("Installing %s %s into %s", t.Name, t.shown(), dir))
	path, release, err := install(ctx, t, dir)
	if err != nil {
		fmt.Println("failed")
		return "", err
	}
	done()
	if v, err := versionOf(ctx, path, t.versionArgs); err != nil || v.String() != strings.TrimPrefix(release, "v") {
		return "", fmt.Errorf("%s was installed but reports %q (%v), not %s", path, v, err, release)
	}
	if t.Pinned == "" {
		ui.Info(fmt.Sprintf("%s %s installed in %s", t.Name, release, dir))
	}
	if !onPath(dir) {
		ui.Hint(fmt.Sprintf("%s isn't on your PATH: add it to use %s yourself (export PATH=\"%s:$PATH\" in ~/.profile)", dir, t.Name, dir))
	}
	return path, nil
}

// Download installs the tool's pinned release into dir, after checking its sha256, and
// returns its path. It never asks: Find does.
func Download(ctx context.Context, t Tool, dir string) (string, error) {
	path, _, err := install(ctx, t, dir)
	return path, err
}

// install downloads the release tow installs (the pinned one, or the newest one named)
// into dir, after checking its sha256, and says which release that was.
func install(ctx context.Context, t Tool, dir string) (path, release string, err error) {
	if release, err = t.release(ctx); err != nil {
		return "", "", err
	}
	sum, err := t.sum(ctx, release)
	if err != nil {
		return "", "", err
	}
	path, err = download(ctx, t, t.url(release, platform), sum, dir)
	return path, release, err
}

// shown is what tow installs, in words: the pinned release, or which newest one.
func (t Tool) shown() string { return cmp.Or(t.Pinned, t.want) }

// release is the release to install: the pinned one, or the newest one latestURL names,
// which must be one the tool's checks accept.
func (t Tool) release(ctx context.Context) (string, error) {
	if t.Pinned != "" {
		return t.Pinned, nil
	}
	body, err := fetch(ctx, t.latestURL)
	if err != nil {
		return "", err
	}
	release := strings.TrimSpace(body)
	if v, err := parseVersion(release); err != nil || !releaseName.MatchString(release) || !t.supports(v) {
		return "", fmt.Errorf("%s names %.40q, not a %s %s release; nothing was installed", t.latestURL, release, t.Name, t.Supports)
	}
	return release, nil
}

// sum is the sha256 the download must have: built into tow for a pinned release, or the
// one the project publishes beside a release tow carries none for.
func (t Tool) sum(ctx context.Context, release string) (string, error) {
	if s, ok := t.sums[platform]; ok && release == t.Pinned {
		return s, nil
	}
	if t.sumURL == nil {
		return "", fmt.Errorf("no tested %s build for %s", t.Name, platform)
	}
	url := t.sumURL(release, platform)
	body, err := fetch(ctx, url)
	if err != nil {
		return "", err
	}
	// The file holds the sum, alone or followed by the file's name.
	fields := strings.Fields(body)
	if len(fields) == 0 || !sha256Hex.MatchString(fields[0]) {
		return "", fmt.Errorf("%s doesn't hold a sha256 (%.80q); nothing was installed", url, body)
	}
	return strings.ToLower(fields[0]), nil
}

// fetch reads a small text file: a sha256, or a release's name.
func fetch(ctx context.Context, url string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	resp, err := get(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	return string(body), nil
}

// getAttempts and getRetryEvery: a request that can't connect (a DNS lookup that timed out,
// seen with WSL's DNS proxy) is tried again; an answer, even an error status, is final.
var (
	getAttempts   = 3
	getRetryEvery = 2 * time.Second
)

// get downloads url, answering 200 OK, or returns why not.
func get(ctx context.Context, url string) (*http.Response, error) {
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				return nil, fmt.Errorf("download %s: %s", url, resp.Status)
			}
			return resp, nil
		}
		if attempt == getAttempts || ctx.Err() != nil {
			return nil, fmt.Errorf("download %s: %w", url, err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("download %s: %w", url, ctx.Err())
		case <-time.After(getRetryEvery):
		}
	}
}

var (
	sha256Hex   = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	releaseName = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
)

func download(ctx context.Context, t Tool, url, sum, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	resp, err := get(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	archive, err := io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	got := sha256.Sum256(archive)
	if hex.EncodeToString(got[:]) != sum {
		return "", fmt.Errorf("%s doesn't match the sha256 tow expects (%s); nothing was installed", url, sum)
	}

	program := archive
	if t.member != nil {
		if program, err = extract(archive, t.member(platform)); err != nil {
			return "", fmt.Errorf("%s: %w", url, err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // a bin folder (~/.local/bin): its programs must be runnable
		return "", err
	}
	// Written next to its final place, then renamed: a half-written program is never run.
	tmp, err := os.CreateTemp(dir, "."+t.Name+"-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(program); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	path := filepath.Join(dir, t.Name)
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

// extract reads one file from a .tar.gz.
func extract(archive []byte, member string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s isn't in the archive", member)
		}
		if err != nil {
			return nil, err
		}
		if h.Name == member && h.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(tr, 200<<20))
		}
	}
}

// candidates are the tool on PATH, then the one in ~/.local/bin when PATH doesn't reach it.
func candidates(name, mine string) []string {
	var out []string
	if p, err := exec.LookPath(name); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		out = append(out, p)
	}
	if info, err := os.Stat(mine); err == nil && !info.IsDir() && !slices.Contains(out, mine) {
		out = append(out, mine)
	}
	return out
}

func onPath(dir string) bool {
	return slices.Contains(filepath.SplitList(os.Getenv("PATH")), dir)
}

// firstOnPath says which of two folders' programs runs when you type the name.
func firstOnPath(dir, other string) string {
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		switch d {
		case dir:
			return fmt.Sprintf("the new one in %s comes first on your PATH, so it's the one you'll run", dir)
		case filepath.Dir(other):
			return "it stays first on your PATH; tow runs the new one by its full path"
		}
	}
	return "tow runs the new one by its full path"
}

// version is a release like 4.3.1.
type version struct{ major, minor, patch int }

func (v version) String() string { return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch) }

var versionPattern = regexp.MustCompile(`v?(\d+)\.(\d+)\.(\d+)`)

// versionOf asks a tool for its release: helm says v4.3.1+gc0d08d0, kubectl "Client
// Version: v1.37.2".
func versionOf(ctx context.Context, path string, args []string) (version, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, args...).Output()
	if err != nil {
		return version{}, err
	}
	return parseVersion(string(out))
}

func parseVersion(s string) (version, error) {
	m := versionPattern.FindStringSubmatch(s)
	if m == nil {
		return version{}, fmt.Errorf("unexpected version %q", strings.TrimSpace(s))
	}
	var v version
	v.major, _ = strconv.Atoi(m[1])
	v.minor, _ = strconv.Atoi(m[2])
	v.patch, _ = strconv.Atoi(m[3])
	return v, nil
}
