package versions

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"
)

// Sources (read 2026-11-13), followed here:
//   - dl.k8s.io/release/stable-1.N.txt names the newest patch of minor N; a release's files
//     are under dl.k8s.io/release/<version>/ (404 when there's no such release)
//   - update.k3s.io/v1-release/channels/v1.N redirects to the newest k3s release of minor N
//     (404 for a minor without one); k3s's releases are GitHub releases of k3s-io/k3s

// Release channels each engine publishes. Vars so tests can point them at a fake server.
var (
	KubernetesStableURL = "https://dl.k8s.io/release/stable.txt"
	K3sStableURL        = "https://update.k3s.io/v1-release/channels/stable"
	// Formats taking a minor (37) or a release (v1.37.2, v1.37.2+k3s1).
	KubernetesMinorURL   = "https://dl.k8s.io/release/stable-1.%d.txt"
	K3sMinorURL          = "https://update.k3s.io/v1-release/channels/v1.%d"
	KubernetesReleaseURL = "https://dl.k8s.io/release/%s/bin/linux/amd64/kubeadm.sha256"
	K3sReleaseURL        = "https://github.com/k3s-io/k3s/releases/tag/%s"
)

// NewestPatch is the newest release of a Kubernetes minor (37) for the engine: v1.37.4, or
// v1.37.4+k3s1 for k3s.
func NewestPatch(ctx context.Context, engine string, minor int) (string, error) {
	if engine == "kubeadm" {
		return kubernetesChannel(ctx, fmt.Sprintf(KubernetesMinorURL, minor))
	}
	return k3sChannel(ctx, fmt.Sprintf(K3sMinorURL, minor))
}

// Exists checks that a release was published: kubeadm's files on dl.k8s.io, or k3s's
// GitHub release.
func Exists(ctx context.Context, engine, release string) error {
	url := fmt.Sprintf(KubernetesReleaseURL, release)
	if engine != "kubeadm" {
		url = fmt.Sprintf(K3sReleaseURL, strings.ReplaceAll(release, "+", "%2B"))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("check that %s exists (%s): %w", release, url, err)
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%s isn't a release (%s answers 404)", release, url)
	case resp.StatusCode >= 400:
		return fmt.Errorf("check that %s exists: %s answered %s", release, url, resp.Status)
	}
	return nil
}

// Resolve turns "stable" into a concrete release for the engine and passes pinned versions through.
// Called at apply time, never while parsing, so config validation stays offline.
func Resolve(ctx context.Context, engine, version string) (string, error) {
	if version != "stable" {
		return version, nil
	}
	switch engine {
	case "kubeadm":
		return kubernetesStable(ctx)
	case "k3s", "k3d": // k3d runs k3s images, so it follows the k3s channel
		return k3sStable(ctx)
	default:
		return "", fmt.Errorf("no release channel known for engine %q", engine)
	}
}

var client = &http.Client{
	Timeout: 30 * time.Second, // generous: Go's own DNS resolver can take seconds per lookup (e.g. under WSL)
	// The k3s channel answers with a redirect to the release; we want the redirect itself.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func kubernetesStable(ctx context.Context) (string, error) {
	return kubernetesChannel(ctx, KubernetesStableURL)
}

func k3sStable(ctx context.Context) (string, error) { return k3sChannel(ctx, K3sStableURL) }

// kubernetesChannel reads a release from one of dl.k8s.io's text files.
func kubernetesChannel(ctx context.Context, url string) (string, error) {
	resp, err := get(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolve stable Kubernetes: %s returned %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", fmt.Errorf("resolve stable Kubernetes: %w", err)
	}
	return checkVersion(strings.TrimSpace(string(body)))
}

// k3sChannel follows a k3s channel: it redirects to .../releases/tag/v1.X.Y+k3sN.
func k3sChannel(ctx context.Context, url string) (string, error) {
	resp, err := get(ctx, url)
	if err != nil {
		return "", err
	}
	resp.Body.Close()

	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("resolve stable k3s: %s returned %s without a redirect", url, resp.Status)
	}
	return checkVersion(path.Base(loc))
}

const attempts = 3

// get retries network errors: flaky DNS (seen with WSL's DNS proxy) often succeeds a second later.
func get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	for attempt := 1; ; attempt++ {
		resp, err := client.Do(req)
		if err == nil {
			return resp, nil
		}
		if attempt == attempts || ctx.Err() != nil {
			return nil, fmt.Errorf("resolve stable version (pin one with 'version:' to work offline): %w", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(attempt) * time.Second):
		}
	}
}

func checkVersion(v string) (string, error) {
	if !strings.HasPrefix(v, "v1.") {
		return "", fmt.Errorf("release channel returned unexpected version %q", v)
	}
	return v, nil
}
