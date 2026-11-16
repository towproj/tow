package k3d

import (
	"slices"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/ui"
)

func TestKubeContext(t *testing.T) {
	if got := KubeContext("dev"); got != "k3d-dev" {
		t.Errorf("KubeContext(dev) = %q, want k3d-dev", got)
	}
}

func args(t *testing.T, spec providers.Spec) string {
	t.Helper()
	return strings.Join(createArgs(spec, nil), " ")
}

func TestCreateArgs(t *testing.T) {
	tests := []struct {
		version string
		want    []string
	}{
		{"", []string{"cluster", "create", "dev"}},
		{"v1.36.5+k3s1", []string{"cluster", "create", "dev", "--image", "rancher/k3s:v1.36.5-k3s1"}},
		{"v1.34.1", []string{"cluster", "create", "dev", "--image", "rancher/k3s:v1.34.1-k3s1"}},
	}
	for _, tt := range tests {
		if got := createArgs(providers.Spec{Name: "dev", Version: tt.version}, nil); !slices.Equal(got, tt.want) {
			t.Errorf("createArgs(%q) = %v, want %v", tt.version, got, tt.want)
		}
	}
}

func TestCreateArgsTopology(t *testing.T) {
	if got := args(t, providers.Spec{Name: "ha", Local: &providers.LocalRequest{Servers: 3, Agents: 2}}); got != "cluster create ha --servers 3 --agents 2" {
		t.Errorf("args = %q", got)
	}
	if got := args(t, providers.Spec{Name: "dev", Local: &providers.LocalRequest{Servers: 1}}); got != "cluster create dev" {
		t.Errorf("k3d's own defaults need no flags: %q", got)
	}
}

// The file's settings become k3s flags on the right nodes, and tow writes the kubeconfig.
func TestCreateArgsFromFile(t *testing.T) {
	k := &providers.KubernetesSpec{
		ServerFlags: []providers.Flag{{Name: "disable", Value: "traefik"}, {Name: "kube-apiserver-arg", Value: "oidc-username-claim=user@example"}},
		AgentFlags:  []providers.Flag{{Name: "node-label", Value: "gpu=true"}},
		Context:     "dev",
		Merge:       true,
	}
	got := args(t, providers.Spec{Name: "dev", Local: &providers.LocalRequest{Servers: 1, Agents: 1}, Kubernetes: k})
	want := `cluster create dev --agents 1 --k3s-arg --disable=traefik@server:* --k3s-arg --kube-apiserver-arg=oidc-username-claim=user\@example@server:* ` +
		`--k3s-arg --node-label=gpu=true@agent:* --kubeconfig-update-default=false --kubeconfig-switch-context=false`
	if got != want {
		t.Errorf("args =\n%s\nwant\n%s", got, want)
	}

	// No agents, no agent flags: a filter matching no node would only confuse k3d.
	got = args(t, providers.Spec{Name: "dev", Local: &providers.LocalRequest{Servers: 1}, Kubernetes: k})
	if strings.Contains(got, "agent:*") {
		t.Errorf("agent flags without agents: %s", got)
	}

}

// A secret is read just before k3d runs and hidden from tow's output; a dry run shows
// only the variable's name.
func TestSecretFlags(t *testing.T) {
	spec := providers.Spec{Name: "dev", Kubernetes: &providers.KubernetesSpec{
		ServerFlags: []providers.Flag{{Name: "etcd-s3-secret-key", Env: "TOW_TEST_S3"}, {Name: "kube-apiserver-arg", Value: "some-flag=", Env: "TOW_TEST_S3"}},
	}}
	if got := args(t, spec); !strings.Contains(got, "--etcd-s3-secret-key=$TOW_TEST_S3@server:*") || !strings.Contains(got, "--kube-apiserver-arg=some-flag=$TOW_TEST_S3@server:*") {
		t.Errorf("dry run must show the variable, not a value: %s", got)
	}

	if _, err := readSecrets(spec); err == nil || !strings.Contains(err.Error(), "TOW_TEST_S3 isn't set") {
		t.Errorf("an unset variable must stop apply, by name: %v", err)
	}
	t.Setenv("TOW_TEST_S3", "fake-s3-secret-7731")
	secrets, err := readSecrets(spec)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(createArgs(spec, secrets), " ")
	if !strings.Contains(got, "--etcd-s3-secret-key=fake-s3-secret-7731@server:*") {
		t.Errorf("k3d must receive the value: %s", got)
	}
	if strings.Contains(ui.Redact(got), "fake-s3-secret-7731") {
		t.Error("once read, the value must be hidden from tow's output")
	}

	// --verbose shows the command line with each value hidden, even one too short for the
	// output's own redaction, and one with an @ (escaped for k3d).
	short := map[string]string{"TOW_TEST_S3": "k3y@1"}
	if s := shown(createArgs(spec, short), short); strings.Contains(s, "k3y") || !strings.Contains(s, "--etcd-s3-secret-key=<$TOW_TEST_S3>@server:*") {
		t.Errorf("verbose command line: %s", s)
	}
}
