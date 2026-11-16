package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/config"
)

// runCLI executes the real command tree; only paths that stop before touching infra are tested here.
func runCLI(t *testing.T, args ...string) error {
	t.Helper()
	rootCmd.SetArgs(args)
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	return rootCmd.Execute()
}

func TestApplyFailsFast(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // the real command: never the user's own ~/.tow
	invalid := filepath.Join(t.TempDir(), "bad.yaml")
	os.WriteFile(invalid, []byte(`
apiVersion: towproj.github.io/v1alpha1
kind: Cluster
metadata: {name: Bad_Name}
spec:
  kubernetes: {distribution: kubeadm, networking: {cni: weave}}
  infrastructure: {provider: byoh, access: {ssh: {user: u}}}
  controlPlane: {machines: [{host: a}]}
`), 0600)
	secret := filepath.Join(t.TempDir(), "secret.yaml")
	os.WriteFile(secret, []byte(`
apiVersion: towproj.github.io/v1alpha1
kind: Cluster
metadata: {name: x}
spec:
  flavor: dev-test
  infrastructure: {provider: aws, aws: {region: eu-west-3, secretAccessKey: abc}}
`), 0600)
	noKey := filepath.Join(t.TempDir(), "nokey.yaml")
	os.WriteFile(noKey, []byte(`
apiVersion: towproj.github.io/v1alpha1
kind: Cluster
metadata: {name: nokey}
spec:
  flavor: homelab
  infrastructure: {provider: byoh, access: {ssh: {user: pi, keyPath: /nonexistent/id_ed25519}}}
  controlPlane: {machines: [{host: 192.0.2.10}]}
`), 0600)

	tests := []struct {
		name string
		file string
		want []string
	}{
		{"missing file", filepath.Join(t.TempDir(), "nope.yaml"), []string{"nope.yaml not found", "examples/"}},
		{"lists every problem", invalid, []string{"bad.yaml is invalid:", "\n  - metadata.name \"Bad_Name\"", "\n  - spec.kubernetes.networking.cni: \"weave\" is unknown"}},
		{"credentials refused", secret, []string{"refusing spec.infrastructure.aws.secretAccessKey, which looks like a credential"}},
		{"byoh ssh key checked before any machine", noKey, []string{"ssh key /nonexistent/id_ed25519"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runCLI(t, "apply", "-f", tt.file)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error missing %q:\n%v", w, err)
				}
			}
		})
	}
}

// The command to run again is the one typed, safe to paste into a shell.
func TestCommandLine(t *testing.T) {
	got := commandLine([]string{"./tow", "apply", "-f", "my lab.yaml", "--yes", "it's"})
	if want := `./tow apply -f 'my lab.yaml' --yes 'it'\''s'`; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestDescribe(t *testing.T) {
	cfg, err := config.Load("../examples/dev-test-aws.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := describe(cfg), "cluster 'dev-aws': kubeadm + calico on aws eu-west-3, 1 control plane + 1 worker (flavor dev-test)"; got != want {
		t.Errorf("describe = %q\nwant       %q", got, want)
	}
	cfg, err = config.Load("../examples/local.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := describe(cfg), "cluster 'dev': k3d + flannel on local, 1 control plane (flavor local)"; got != want {
		t.Errorf("describe = %q\nwant       %q", got, want)
	}
	cfg, err = config.Parse([]byte("apiVersion: towproj.github.io/v1alpha1\nkind: Cluster\nmetadata: {name: bare}\nspec: {infrastructure: {provider: local}, kubernetes: {networking: {cni: none}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := describe(cfg), "cluster 'bare': k3d (no CNI) on local, 1 control plane"; got != want {
		t.Errorf("describe = %q\nwant       %q", got, want)
	}
}
