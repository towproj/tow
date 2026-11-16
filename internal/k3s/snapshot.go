package k3s

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/remote"
)

// Sources (read 2026-11-13), followed here:
//   - k3s etcd-snapshot save takes an on-demand snapshot of the embedded etcd, into
//     ${data-dir}/server/db/snapshots unless etcd-snapshot-dir says otherwise; --name is the
//     prefix of its file  https://docs.k3s.io/cli/etcd-snapshot

// defaultDataDir is k3s's data-dir when it runs as root.
const defaultDataDir = "/var/lib/rancher/k3s"

// SnapshotDir is where the servers keep their snapshots: etcd-snapshot-dir, else the
// snapshots folder in data-dir, as the file sets them.
func SnapshotDir(cfg *config.Config) string {
	args := cfg.Spec.Kubernetes.K3s.Server.ExtraArgs
	if v, ok := args["etcd-snapshot-dir"]; ok && len(v.Values) == 1 {
		return v.Values[0]
	}
	data := defaultDataDir
	if v, ok := args["data-dir"]; ok && len(v.Values) == 1 {
		data = v.Values[0]
	}
	return path.Join(data, "server/db/snapshots")
}

// Snapshot takes an etcd snapshot now on a server, into dir (k3s's own snapshot folder), and
// returns its path there. k3s's own schedule keeps its snapshots as they are.
func Snapshot(ctx context.Context, n providers.Node, knownHosts, dir string, connect func(ctx context.Context, n providers.Node, knownHosts string) (prepare.Runner, error)) (string, error) {
	if connect == nil {
		connect = prepare.Connect
	}
	r, err := connect(ctx, n, knownHosts)
	if err != nil {
		return "", err
	}
	defer r.Close()
	script := fmt.Sprintf(`set -e
out=$(k3s etcd-snapshot save --name tow 2>&1) || { echo "$out"; exit 1; }
ls -1t %s/tow-* | head -n 1
`, remote.Quote(dir))
	out, err := r.RunInput(ctx, asRoot, script)
	if err != nil {
		return "", fmt.Errorf("%s: k3s etcd-snapshot save: %w\n%s", n.Label(), err, strings.TrimSpace(out))
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return lines[len(lines)-1], nil
}
