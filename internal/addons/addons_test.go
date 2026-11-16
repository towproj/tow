package addons

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/config"
	"gopkg.in/yaml.v3"
)

// helmCall is one command the fake helm ran, with the files it was given as they were then.
type helmCall struct {
	args  []string
	files map[string]string // path → content
}

func useHelm(t *testing.T, fail error) *[]helmCall {
	t.Helper()
	var calls []helmCall
	orig := run
	run = func(_ context.Context, helm string, args []string, _ string) (string, string, error) {
		c := helmCall{args: args, files: map[string]string{}}
		for i, a := range args {
			path := ""
			switch {
			case i > 0 && args[i-1] == "--values":
				path = a
			case i > 0 && args[i-1] == "--set-file":
				_, path, _ = strings.Cut(a, "=")
			}
			if path != "" {
				data, err := os.ReadFile(path)
				info, _ := os.Stat(path)
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Errorf("values file %s: %v, mode %v", path, err, info.Mode())
				}
				c.files[path] = string(data)
			}
		}
		calls = append(calls, c)
		if fail != nil {
			return "", "Error: INSTALLATION FAILED: chart \"nope\" not found in https://example.com repository", fail
		}
		return "Release \"x\" has been upgraded. Happy Helming!", "", nil
	}
	t.Cleanup(func() { run = orig })
	return &calls
}

var charts = []config.HelmChart{
	{Name: "podinfo", Repo: "https://stefanprodan.github.io/podinfo", Chart: "podinfo", Version: "6.17.0", Namespace: "demo",
		Values: map[string]any{"replicaCount": 2, "ui": map[string]any{"color": "#34577c"}}},
	{Name: "db", Repo: "oci://registry.example.com/charts/", Chart: "postgres", Version: "1.2.3", Namespace: "data",
		ValuesFromEnv: map[string]string{"auth.password": "DB_PASSWORD", "auth.users[0].password": "DB_USER_PASSWORD"}},
}

func options(dir string) Options {
	return Options{Helm: "/usr/bin/helm", Kubeconfig: "/state/lab/kubeconfig", Dir: dir, Charts: charts,
		Secrets: map[string]string{"DB_PASSWORD": "s3cret-password", "DB_USER_PASSWORD": "user-password"}}
}

func TestInstall(t *testing.T) {
	calls := useHelm(t, nil)
	dir := t.TempDir()
	if err := Install(context.Background(), options(dir)); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 {
		t.Fatalf("calls = %d", len(*calls))
	}

	// An https repository: the chart by name, with --repo; the values in a file.
	first := strings.Join((*calls)[0].args, " ")
	if !strings.HasPrefix(first, "upgrade --install podinfo podinfo --repo https://stefanprodan.github.io/podinfo --version 6.17.0 --namespace demo --create-namespace --kubeconfig /state/lab/kubeconfig --wait --timeout 10m --values "+dir+"/values-podinfo-") {
		t.Errorf("podinfo: helm %s", first)
	}
	for _, content := range (*calls)[0].files {
		var got map[string]any
		if err := yaml.Unmarshal([]byte(content), &got); err != nil || got["replicaCount"] != 2 || got["ui"].(map[string]any)["color"] != "#34577c" {
			t.Errorf("values file: %v, %q", err, content)
		}
	}

	// OCI: the full reference, no --repo; each secret in its own file, never on the command line.
	second := strings.Join((*calls)[1].args, " ")
	if !strings.HasPrefix(second, "upgrade --install db oci://registry.example.com/charts/postgres --version 1.2.3 --namespace data") || strings.Contains(second, "--repo") || strings.Contains(second, "--values") {
		t.Errorf("db: helm %s", second)
	}
	if strings.Contains(second, "password\n") || strings.Contains(second, "s3cret") || strings.Count(second, "--set-file auth.") != 2 || !strings.Contains(second, "--set-file auth.users[0].password=") {
		t.Errorf("db secrets: helm %s", second)
	}
	var secrets []string
	for _, content := range (*calls)[1].files {
		secrets = append(secrets, content)
	}
	if len(secrets) != 2 || !strings.Contains(strings.Join(secrets, ","), "s3cret-password") {
		t.Errorf("secret files: %v", secrets)
	}

	// Every file is gone once helm is done.
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
}

func TestInstallStopsAtTheFirstFailure(t *testing.T) {
	calls := useHelm(t, errors.New("exit status 1"))
	dir := t.TempDir()
	err := Install(context.Background(), options(dir))
	if err == nil || !strings.Contains(err.Error(), "helm chart podinfo: exit status 1") || !strings.Contains(err.Error(), "INSTALLATION FAILED") {
		t.Errorf("err = %v", err)
	}
	if len(*calls) != 1 {
		t.Errorf("charts after a failure must wait: %d calls", len(*calls))
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
}

func TestMissingSecret(t *testing.T) {
	useHelm(t, nil)
	o := options(t.TempDir())
	delete(o.Secrets, "DB_PASSWORD")
	if err := Install(context.Background(), o); err == nil || !strings.Contains(err.Error(), "DB_PASSWORD wasn't read") {
		t.Errorf("err = %v", err)
	}
}

func TestCommandsShowNoSecret(t *testing.T) {
	got := strings.Join(Commands(options("/state/lab")), "\n")
	for _, want := range []string{
		"helm upgrade --install podinfo podinfo --repo https://stefanprodan.github.io/podinfo",
		"--values <the chart's values>",
		"--set-file auth.password=<from $DB_PASSWORD>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dry run lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "s3cret") {
		t.Errorf("dry run shows a secret:\n%s", got)
	}
	k := config.Kubernetes{Addons: config.Addons{HelmCharts: charts},
		Storage: config.Storage{CSI: []string{config.CSISMB}, SMB: &config.SMB{UsernameEnv: "SMB_USER", PasswordEnv: "SMB_PASSWORD"}}}
	if got := Secrets(k); strings.Join(got, ",") != "DB_PASSWORD,DB_USER_PASSWORD,SMB_PASSWORD,SMB_USER" {
		t.Errorf("secret variables = %v", got)
	}
}
