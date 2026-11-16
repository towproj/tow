package core

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/k3s"
	"github.com/towproj/tow/internal/kubeadm"
	"github.com/towproj/tow/internal/kubecfg"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/ui"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// Sources (read 2026-11-13), followed here:
//   - kubeadm's certificates last a year; 'kubeadm upgrade' renews them; 'kubeadm certs renew
//     all' renews them now, then the control plane's static pods must restart (their manifests
//     moved out of /etc/kubernetes/manifests and back); copies of admin.conf must be refreshed
//     https://kubernetes.io/docs/tasks/administer-cluster/kubeadm/kubeadm-certs/
//   - k3s's certificates last 365 days and are renewed when k3s starts within 120 days of
//     expiring; 'k3s certificate rotate' (k3s stopped) renews them now
//     https://docs.k3s.io/cli/certificate

// renewWithin is how close to expiring a certificate gets before apply renews it.
const renewWithin = 30 * 24 * time.Hour

// apiCertExpiry reads when the API server's certificate expires, from its TLS handshake;
// nothing is sent over the connection. Go checks the certificate as kubectl does (signed by
// the kubeconfig's certificate authority, issued for the server's name). An expired one must
// still be read, since apply renews it: then the handshake is made again, checked at the
// certificate's last valid second, so everything but the date is still checked. A
// kubeconfig that opts out of checking (insecure-skip-tls-verify) is followed, as kubectl
// follows it.
func apiCertExpiry(ctx context.Context, cluster *clientcmdapi.Cluster) (time.Time, error) {
	server, err := url.Parse(cluster.Server)
	if err != nil {
		return time.Time{}, err
	}
	name := server.Hostname()
	if cluster.TLSServerName != "" {
		name = cluster.TLSServerName
	}
	var roots *x509.CertPool // nil: the system's
	ca := cluster.CertificateAuthorityData
	if len(ca) == 0 && cluster.CertificateAuthority != "" {
		if ca, err = os.ReadFile(cluster.CertificateAuthority); err != nil {
			return time.Time{}, err
		}
	}
	if len(ca) > 0 {
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(ca) {
			return time.Time{}, errors.New("the kubeconfig's certificate authority holds no certificate")
		}
	}
	conf := &tls.Config{RootCAs: roots, ServerName: name, InsecureSkipVerify: cluster.InsecureSkipTLSVerify}
	handshake := func() (time.Time, error) {
		dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: conf}
		conn, err := dialer.DialContext(ctx, "tcp", server.Host)
		if err != nil {
			return time.Time{}, err
		}
		defer conn.Close()
		certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
		if len(certs) == 0 {
			return time.Time{}, errors.New("the API server sent no certificate")
		}
		return certs[0].NotAfter, nil
	}
	notAfter, err := handshake()
	var invalid x509.CertificateInvalidError
	if errors.As(err, &invalid) && invalid.Reason == x509.Expired {
		last := invalid.Cert.NotAfter.Add(-time.Second)
		conf.Time = func() time.Time { return last }
		notAfter, err = handshake()
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("the API server's certificate: %w", err)
	}
	return notAfter, nil
}

// Swappable so tests run without a cluster.
var (
	// certExpiry reads when the API server's certificate expires (from its TLS handshake)
	// and when the kubeconfig's client certificate does; zero when there's none.
	certExpiry = func(ctx context.Context, kubeconfig string) (api, client time.Time, err error) {
		cfg, err := clientcmd.LoadFromFile(kubeconfig)
		if err != nil {
			return api, client, err
		}
		c := cfg.Contexts[cfg.CurrentContext]
		if c == nil || cfg.Clusters[c.Cluster] == nil {
			return api, client, fmt.Errorf("%s has no current context", kubeconfig)
		}
		if u := cfg.AuthInfos[c.AuthInfo]; u != nil {
			if block, _ := pem.Decode(u.ClientCertificateData); block != nil {
				if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
					client = cert.NotAfter
				}
			}
		}
		api, err = apiCertExpiry(ctx, cfg.Clusters[c.Cluster])
		return api, client, err
	}
	// fetchAdminKubeconfig reads the cluster's admin kubeconfig on a control plane.
	fetchAdminKubeconfig = func(ctx context.Context, n providers.Node, known, file string) ([]byte, error) {
		out, err := runOn(ctx, n, known, "cat "+file+"\n")
		if err != nil {
			return nil, fmt.Errorf("read %s on %s: %w", file, n.Label(), err)
		}
		return []byte(out), nil
	}
	// renewOn runs a control plane's or worker's part of a renewal.
	renewOn = func(ctx context.Context, n providers.Node, known, script string) error {
		_, err := runOn(ctx, n, known, script)
		return err
	}
)

// expiryNote says when a certificate expires: "until 2027-11-13 (364 days)".
func expiryNote(t time.Time, now time.Time) string {
	days := int(t.Sub(now).Hours() / 24)
	if t.Before(now) {
		return fmt.Sprintf("expired %s", t.Local().Format("2006-01-02"))
	}
	return fmt.Sprintf("until %s (%s)", t.Local().Format("2006-01-02"), ui.Plural(days, "day"))
}

// planCerts plans renewing the cluster's certificates when the API server's or the
// kubeconfig's expires within 30 days, or when asked (apply --renew-certificates). An
// upgrade renews them itself.
func (o *Orchestrator) planCerts(ctx context.Context, plan *updatePlan, cfg *config.Config, prev *state.ClusterState, up *upgrade) error {
	if cfg.Mode == config.ModeLocal || prev.Outputs == nil || prev.Outputs.Kubeconfig == "" {
		return nil
	}
	api, client, err := certExpiry(ctx, prev.Outputs.Kubeconfig)
	if err != nil {
		ui.Debug("certificates: %v", err)
	}
	now := time.Now()
	var why []string
	for what, t := range map[string]time.Time{"the API server's": api, "your kubeconfig's": client} {
		if !t.IsZero() && t.Sub(now) < renewWithin {
			why = append(why, what+" "+expiryNote(t, now))
		}
	}
	if len(why) == 0 && !o.renewCerts {
		return nil
	}
	if up != nil {
		plan.notes = append(plan.notes, "the upgrade renews the cluster's certificates itself")
		return nil
	}
	reason := "asked (--renew-certificates)"
	if len(why) > 0 {
		reason = strings.Join(why, "; ")
	}
	lines := []string{"Renew the cluster's certificates: " + reason}
	if cfg.Distribution() == config.DistributionKubeadm {
		lines = append(lines, "on each control plane in turn: 'kubeadm certs renew all', then its control plane restarted (static pods moved out and back) and checked")
	} else {
		lines = append(lines, "on each control plane in turn: k3s stopped, 'k3s certificate rotate', k3s started and checked; then each worker's k3s restarted (it renews its own within 120 days of expiring)")
	}
	lines = append(lines, "then your kubeconfig is fetched again (its client certificate is renewed too); pods keep running")
	plan.cluster = append(plan.cluster, planStep{lines: lines, run: func(ctx context.Context) error { return o.renewCertificates(ctx, cfg, prev) }})
	return nil
}

// renewCertificates renews on every control plane in turn (and restarts k3s's workers),
// then refreshes the kubeconfig tow handed out.
func (o *Orchestrator) renewCertificates(ctx context.Context, cfg *config.Config, prev *state.ClusterState) error {
	known := filepath.Join(state.Dir(cfg.Name()), "known_hosts")
	var cps, workers []providers.Node
	for _, n := range prev.Outputs.Nodes {
		if n.Role == config.RoleControlPlane {
			cps = append(cps, n)
		} else {
			workers = append(workers, n)
		}
	}
	if len(cps) == 0 {
		return errors.New("the cluster has no control plane in tow's state")
	}
	kubeadmCluster := cfg.Distribution() == config.DistributionKubeadm
	for _, n := range cps {
		script := "set -e\nsystemctl stop k3s\nk3s certificate rotate\nsystemctl start k3s\n" + k3s.ReadyScript
		if kubeadmCluster {
			script = "set -e\nkubeadm certs renew all\n" + kubeadm.RestartControlPlaneScript(cfg.Spec.Kubernetes.ContainerRuntime)
		}
		if err := ui.StepRun(fmt.Sprintf("Renewing the certificates on %s", n.Label()), func() error { return renewOn(ctx, n, known, script) }); err != nil {
			return err
		}
		if err := ui.StepRun("Checking the API on every control plane", func() error { return controlPlanesHealthy(ctx, cfg, cps, known) }); err != nil {
			return err
		}
	}
	if !kubeadmCluster {
		for _, n := range workers {
			if err := ui.StepRun(fmt.Sprintf("Restarting k3s on %s", n.Label()), func() error {
				return renewOn(ctx, n, known, "systemctl restart k3s-agent\n")
			}); err != nil {
				return err
			}
		}
	}
	return ui.StepRun("Fetching your kubeconfig again", func() error { return refreshKubeconfig(ctx, cfg, prev, cps[0], known) })
}

// refreshKubeconfig reads the admin kubeconfig on a control plane again (its client
// certificate renewed), points it where the one tow handed out points, and stores it
// the same way: in the state folder, and merged into yours when it was.
func refreshKubeconfig(ctx context.Context, cfg *config.Config, prev *state.ClusterState, cp providers.Node, known string) error {
	file := "/etc/rancher/k3s/k3s.yaml"
	if cfg.Distribution() == config.DistributionKubeadm {
		file = "/etc/kubernetes/admin.conf"
	}
	data, err := fetchAdminKubeconfig(ctx, cp, known, file)
	if err != nil {
		return err
	}
	current, err := clientcmd.LoadFromFile(prev.Outputs.Kubeconfig)
	if err != nil {
		return err
	}
	c := current.Contexts[current.CurrentContext]
	if c == nil || current.Clusters[c.Cluster] == nil {
		return fmt.Errorf("%s has no current context", prev.Outputs.Kubeconfig)
	}
	if data, err = kubecfg.Point(data, current.Clusters[c.Cluster].Server); err != nil {
		return err
	}
	merge := prev.Spec != nil && prev.Spec.Kubernetes != nil && prev.Spec.Kubernetes.Merge
	return kubecfg.Store(data, prev.Outputs.KubeContext, prev.Outputs.Kubeconfig, merge)
}
