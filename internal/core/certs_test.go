package core

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/kubeadm"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/runtimes"
	"github.com/towproj/tow/internal/state"
	"k8s.io/client-go/tools/clientcmd"
)

// clientCert is a PEM client certificate that expires at notAfter.
func clientCert(t *testing.T, notAfter time.Time) []byte {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "admin"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func kubeconfigFor(server string, cert []byte) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters: [{name: lab, cluster: {server: %q, insecure-skip-tls-verify: true}}]
users: [{name: lab, user: {client-certificate-data: %s, client-key-data: a2V5}}]
contexts: [{name: lab, context: {cluster: lab, user: lab}}]
current-context: lab
`, server, base64.StdEncoding.EncodeToString(cert))
}

// The API server's certificate is read from its TLS handshake, the client's from the
// kubeconfig.
func TestCertExpiry(t *testing.T) {
	srv := httptest.NewTLSServer(nil)
	defer srv.Close()
	in := time.Now().Add(20 * 24 * time.Hour).Truncate(time.Second)
	kc := filepath.Join(t.TempDir(), "kubeconfig")
	os.WriteFile(kc, []byte(kubeconfigFor(srv.URL, clientCert(t, in))), 0o600)
	api, client, err := certExpiry(context.Background(), kc)
	if err != nil {
		t.Fatal(err)
	}
	if !api.Equal(srv.Certificate().NotAfter) || !client.Equal(in.UTC()) {
		t.Errorf("api %v (want %v), client %v (want %v)", api, srv.Certificate().NotAfter, client, in)
	}
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	if got := certificatesLine(now.Add(364*24*time.Hour), now.Add(20*24*time.Hour), now); !strings.Contains(got, "the API server's until 2027-11-12 (364 days); your kubeconfig's until 2026-12-03 (20 days) (soon: 'tow apply' renews them") {
		t.Errorf("line: %s", got)
	}
	if got := certificatesLine(now.Add(-time.Hour), time.Time{}, now); !strings.HasPrefix(got, "Certificates: the API server's expired") {
		t.Errorf("expired: %s", got)
	}
}

// useRenewal fakes the machines' side of a renewal.
func useRenewal(t *testing.T, api, client time.Time) *[]string {
	t.Helper()
	var calls []string
	origExpiry, origFetch, origRenew, origHealthy := certExpiry, fetchAdminKubeconfig, renewOn, controlPlanesHealthy
	certExpiry = func(context.Context, string) (time.Time, time.Time, error) { return api, client, nil }
	fetchAdminKubeconfig = func(_ context.Context, n providers.Node, _, file string) ([]byte, error) {
		calls = append(calls, "fetch "+n.Name+" "+file)
		return []byte(kubeconfigFor("https://127.0.0.1:6443", clientCert(t, time.Now().Add(365*24*time.Hour)))), nil
	}
	renewOn = func(_ context.Context, n providers.Node, _, script string) error {
		first, _, _ := strings.Cut(strings.TrimPrefix(script, "set -e\n"), "\n")
		calls = append(calls, n.Name+": "+first)
		return nil
	}
	controlPlanesHealthy = func(context.Context, *config.Config, []providers.Node, string) error {
		calls = append(calls, "healthy")
		return nil
	}
	t.Cleanup(func() {
		certExpiry, fetchAdminKubeconfig, renewOn, controlPlanesHealthy = origExpiry, origFetch, origRenew, origHealthy
	})
	return &calls
}

// Within 30 days, apply renews: k3s rotates on each control plane, then restarts its
// workers; the kubeconfig is fetched again, keeping its context name and address.
func TestRenewK3s(t *testing.T) {
	useFakes(t)
	useNodes(t)
	soon := time.Now().Add(10 * 24 * time.Hour)
	calls := useRenewal(t, time.Now().Add(300*24*time.Hour), soon)
	cfg := parse(t, "lab", fmt.Sprintf(lab, "", ""))
	running(t, cfg)
	if err := applyYes(t, cfg); err != nil {
		t.Fatal(err)
	}
	want := []string{"cp: systemctl stop k3s", "healthy", "w1: systemctl restart k3s-agent", "fetch cp /etc/rancher/k3s/k3s.yaml"}
	if !slices.Equal(*calls, want) {
		t.Errorf("calls %v, want %v", *calls, want)
	}
	st, _ := state.Load("lab")
	kc, err := clientcmd.LoadFromFile(st.Outputs.Kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	if kc.CurrentContext != "lab" || kc.Clusters[kc.Contexts["lab"].Cluster].Server != "https://10.0.0.1:6443" {
		t.Errorf("kubeconfig: context %s, server %s", kc.CurrentContext, kc.Clusters[kc.Contexts["lab"].Cluster].Server)
	}

	// Nothing near: nothing to do; unless asked.
	*calls = nil
	useRenewal(t, time.Now().Add(300*24*time.Hour), time.Now().Add(300*24*time.Hour))
	if err := applyYes(t, cfg); err != nil || len(*calls) != 0 {
		t.Errorf("not due: %v %v", err, *calls)
	}
	calls = useRenewal(t, time.Now().Add(300*24*time.Hour), time.Now().Add(300*24*time.Hour))
	if err := NewOrchestrator(false).AssumeYes().WithCertRenewal().Apply(context.Background(), cfg); err != nil || len(*calls) != 4 {
		t.Errorf("asked: %v %v", err, *calls)
	}
}

// kubeadm: certs renew all on each control plane, its static pods restarted, checked; the
// workers' certificates rotate themselves (kubelet); admin.conf fetched again.
func TestRenewKubeadm(t *testing.T) {
	useFakes(t)
	useNodes(t)
	calls := useRenewal(t, time.Now().Add(5*24*time.Hour), time.Now().Add(5*24*time.Hour))
	cfg := parse(t, "lab", strings.Replace(fmt.Sprintf(lab, "", ""), "distribution: k3s", "distribution: kubeadm", 1))
	running(t, cfg)
	if err := applyYes(t, cfg); err != nil {
		t.Fatal(err)
	}
	want := []string{"cp: kubeadm certs renew all", "healthy", "fetch cp /etc/kubernetes/admin.conf"}
	if !slices.Equal(*calls, want) {
		t.Errorf("calls %v, want %v", *calls, want)
	}
	s := kubeadm.RestartControlPlaneScript(runtimes.Containerd)
	if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil || !strings.Contains(s, "mv \"$f\" /etc/kubernetes/manifests/") {
		t.Errorf("restart script: %v %s", err, out)
	}
}

// An upgrade renews them itself: only a note.
func TestPlanCertsWithUpgrade(t *testing.T) {
	useFakes(t)
	useRenewal(t, time.Now().Add(5*24*time.Hour), time.Time{})
	cfg := parse(t, "lab", fmt.Sprintf(lab, "", ""))
	running(t, cfg)
	st, _ := state.Load("lab")
	var plan updatePlan
	if err := NewOrchestrator(true).planCerts(context.Background(), &plan, cfg, st, &upgrade{to: "v1.38.0"}); err != nil {
		t.Fatal(err)
	}
	if len(plan.cluster) != 0 || len(plan.notes) != 1 || !strings.Contains(plan.notes[0], "the upgrade renews") {
		t.Errorf("%+v", plan)
	}
}
