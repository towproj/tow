package versions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/k8s":
			w.Write([]byte("v1.34.2\n"))
		case "/k3s":
			http.Redirect(w, r, "https://github.com/k3s-io/k3s/releases/tag/v1.33.6+k3s1", http.StatusFound)
		}
	}))
	defer srv.Close()
	KubernetesStableURL, K3sStableURL = srv.URL+"/k8s", srv.URL+"/k3s"

	tests := []struct{ engine, version, want string }{
		{"kubeadm", "stable", "v1.34.2"},
		{"k3s", "stable", "v1.33.6+k3s1"},
		{"k3d", "stable", "v1.33.6+k3s1"},
		{"kubeadm", "v1.32.0", "v1.32.0"}, // pinned versions never hit the network
	}
	for _, tt := range tests {
		got, err := Resolve(context.Background(), tt.engine, tt.version)
		if err != nil || got != tt.want {
			t.Errorf("Resolve(%s, %s) = %q, %v; want %q", tt.engine, tt.version, got, err, tt.want)
		}
	}
}

// A minor's newest patch, and whether a release exists, for each engine.
func TestNewestPatchAndExists(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		switch r.URL.Path {
		case "/k8s/stable-1.37.txt":
			w.Write([]byte("v1.37.4\n"))
		case "/k3s/v1.37":
			http.Redirect(w, r, "https://github.com/k3s-io/k3s/releases/tag/v1.37.4+k3s1", http.StatusFound)
		case "/k8s/v1.37.4/kubeadm.sha256", "/k3s/tag/v1.37.4+k3s1":
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	KubernetesMinorURL, K3sMinorURL = srv.URL+"/k8s/stable-1.%d.txt", srv.URL+"/k3s/v1.%d"
	KubernetesReleaseURL, K3sReleaseURL = srv.URL+"/k8s/%s/kubeadm.sha256", srv.URL+"/k3s/tag/%s"
	ctx := context.Background()

	if v, err := NewestPatch(ctx, "kubeadm", 37); v != "v1.37.4" || err != nil {
		t.Errorf("kubeadm: %q, %v", v, err)
	}
	if v, err := NewestPatch(ctx, "k3s", 37); v != "v1.37.4+k3s1" || err != nil {
		t.Errorf("k3s: %q, %v", v, err)
	}
	if _, err := NewestPatch(ctx, "k3s", 99); err == nil {
		t.Error("a minor without releases")
	}
	if err := Exists(ctx, "kubeadm", "v1.37.4"); err != nil {
		t.Error(err)
	}
	if err := Exists(ctx, "k3s", "v1.37.4+k3s1"); err != nil {
		t.Error(err)
	}
	if err := Exists(ctx, "k3s", "v1.37.99+k3s1"); err == nil || !strings.Contains(err.Error(), "v1.37.99+k3s1 isn't a release") {
		t.Errorf("missing release: %v", err)
	}
	if methods[len(methods)-1] != http.MethodHead {
		t.Error("Exists only asks for the headers")
	}
}

func TestResolveRetriesNetworkErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("v1.34.2"))
	}))
	defer srv.Close()

	// First attempt hits a closed port, mimicking a DNS/connect failure; later ones succeed.
	dead := httptest.NewServer(nil)
	deadURL := dead.URL
	dead.Close()

	calls := 0
	orig := client.Transport
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		target := srv.URL
		if calls == 1 {
			target = deadURL
		}
		req, _ := http.NewRequestWithContext(r.Context(), r.Method, target, nil)
		return http.DefaultTransport.RoundTrip(req)
	})
	defer func() { client.Transport = orig }()
	KubernetesStableURL = "http://k8s.invalid/stable.txt"

	got, err := Resolve(context.Background(), "kubeadm", "stable")
	if err != nil || got != "v1.34.2" || calls != 2 {
		t.Errorf("got %q, %v after %d calls; want v1.34.2 after 2", got, err, calls)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestResolveRejectsGarbage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>maintenance</html>"))
	}))
	defer srv.Close()
	KubernetesStableURL = srv.URL

	if _, err := Resolve(context.Background(), "kubeadm", "stable"); err == nil {
		t.Fatal("expected error for non-version response")
	}
}
