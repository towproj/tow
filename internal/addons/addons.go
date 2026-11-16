// Package addons installs what the file asks for beyond Kubernetes itself, with the tools
// the user knows: Helm charts with helm, from this machine, through the cluster's kubeconfig.
// What each chart and its values mean is Helm's to judge; its own error is shown as it is.
package addons

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/secretenv"
	"github.com/towproj/tow/internal/ui"
	"gopkg.in/yaml.v3"
)

// Helm waits this long for a chart's resources to be ready.
const chartTimeout = "10m"

// Options say which add-ons go on which cluster.
type Options struct {
	Helm, Kubectl string             // the tools' paths
	Kubeconfig    string             // the cluster's, in the state folder
	Dir           string             // the state folder: values files live there, briefly
	Kubernetes    config.Kubernetes  // what the file asks for
	Charts        []config.HelmChart // in the file's order
	Secrets       map[string]string  // values of the variables the file names, read before anything started
	// Nodes are the cluster's machines, known once they exist (kubelet-csr-approver allows
	// their names and addresses only).
	Nodes []providers.Node
	// ManifestPath resolves a local manifest against the cluster file's folder.
	ManifestPath func(string) string
	Cluster      string   // the cluster's name: EBS volumes' tag, the Proxmox CSI plugin's region
	Proxmox      *Proxmox // where the Proxmox CSI plugin's disks go (provider proxmox)
	// CloudStorage is what the cloud's storage driver needs from the machines' module
	// (providers.Result.StorageConfig: Azure Disk's identity and resource group). IDs only.
	CloudStorage map[string]string
	// Applied, when set, hears the objects each manifest made, for state: what a later
	// apply deletes when the manifest drops them or leaves the file.
	Applied func(path string, objs []Object)
	// Running are the versions of the built-in add-ons that run, by name: a running cluster
	// keeps them unless the file pins another.
	Running map[string]string
}

// run starts helm or kubectl with stdin, and returns what it printed and its errors apart:
// a rendered manifest must not mix with warnings. Swappable so tests run neither.
var run = func(ctx context.Context, program string, args []string, stdin string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Env = secretenv.Environ() // never the variables holding the file's secrets
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	ui.Debug("exec: %s %s", program, strings.Join(args, " "))
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

// Install installs what the file asks for beyond Kubernetes and its network, one after the
// other, each waited for: storage, metrics-server, the Helm charts, then the manifests.
// Running it again changes nothing that's already as asked.
func Install(ctx context.Context, o Options) error {
	r := runner{helm: o.Helm, kubectl: o.Kubectl, kubeconfig: o.Kubeconfig, dir: o.Dir}
	first, _ := o.components() // the manifests, its last part, are applied below
	for _, c := range first {
		done := ui.Step("Installing " + strings.TrimSpace(c.name+" "+c.version))
		if err := r.steps(ctx, c.name, c.steps); err != nil {
			fmt.Println("failed")
			return err
		}
		done()
	}
	for _, h := range o.Charts {
		done := ui.Step(fmt.Sprintf("Installing Helm chart %s %s into %s", h.Name, h.Version, h.Namespace))
		if err := o.install(ctx, h); err != nil {
			fmt.Println("failed")
			return err
		}
		done()
	}
	// The manifests last (components' last part), each one's objects heard by Applied.
	for _, m := range o.Kubernetes.Addons.Manifests {
		done := ui.Step("Installing manifest " + m.Path)
		objs, err := o.ApplyManifest(ctx, m)
		if err != nil {
			fmt.Println("failed")
			return err
		}
		if o.Applied != nil {
			o.Applied(m.Path, objs)
		}
		done()
	}
	return nil
}

func (o Options) install(ctx context.Context, h config.HelmChart) error {
	files, cleanup, err := o.valuesFiles(h)
	defer cleanup()
	if err != nil {
		return err
	}
	out, errOut, err := run(ctx, o.Helm, forceOnHelm4(ctx, o.Helm, args(h, o.Kubeconfig, files)), "")
	if err != nil {
		return fmt.Errorf("helm chart %s: %w\n%s", h.Name, err, strings.TrimSpace(out+"\n"+errOut))
	}
	return nil
}

// files are a chart's values on disk: the file's values, and one file per secret value,
// which helm reads with --set-file. Only their paths reach the command line; they're
// owner-only and deleted once helm is done.
type files struct {
	values  string
	secrets map[string]string // value path → file
}

func (o Options) valuesFiles(h config.HelmChart) (files, func(), error) {
	var f files
	var paths []string
	cleanup := func() {
		for _, p := range paths {
			os.Remove(p)
		}
	}
	write := func(data []byte) (string, error) {
		tmp, err := os.CreateTemp(o.Dir, "values-"+h.Name+"-*.yaml") // 0600
		if err != nil {
			return "", err
		}
		paths = append(paths, tmp.Name())
		if _, err := tmp.Write(data); err != nil {
			tmp.Close()
			return "", err
		}
		return tmp.Name(), tmp.Close()
	}

	if len(h.Values) > 0 {
		data, err := yaml.Marshal(h.Values)
		if err != nil {
			return f, cleanup, err
		}
		if f.values, err = write(data); err != nil {
			return f, cleanup, err
		}
	}
	for _, key := range sortedKeys(h.ValuesFromEnv) {
		secret, ok := o.Secrets[h.ValuesFromEnv[key]]
		if !ok {
			return f, cleanup, fmt.Errorf("environment variable %s wasn't read before the install", h.ValuesFromEnv[key])
		}
		path, err := write([]byte(secret))
		if err != nil {
			return f, cleanup, err
		}
		if f.secrets == nil {
			f.secrets = map[string]string{}
		}
		f.secrets[key] = path
	}
	return f, cleanup, nil
}

// args is helm's command line. An OCI chart is named by its full reference; a chart from an
// https repository by its name and --repo.
func args(h config.HelmChart, kubeconfig string, f files) []string {
	ref, repo := h.Chart, h.Repo
	if strings.HasPrefix(h.Repo, "oci://") {
		ref, repo = strings.TrimSuffix(h.Repo, "/")+"/"+h.Chart, ""
	}
	a := []string{"upgrade", "--install", h.Name, ref}
	if repo != "" {
		a = append(a, "--repo", repo)
	}
	a = append(a, "--version", h.Version, "--namespace", h.Namespace, "--create-namespace",
		"--kubeconfig", kubeconfig, "--wait", "--timeout", chartTimeout)
	if f.values != "" {
		a = append(a, "--values", f.values)
	}
	for _, key := range sortedKeys(f.secrets) {
		a = append(a, "--set-file", key+"="+f.secrets[key])
	}
	return a
}

// Commands are the commands Install would run, for a dry run: values files show as
// placeholders, secrets by the variable they come from.
func Commands(o Options) []string {
	var out []string
	first, last := o.components()
	describeAll := func(cs []component) {
		for _, c := range cs {
			out = append(out, fmt.Sprintf("%s:", strings.TrimSpace(c.name+" "+c.version)))
			for _, line := range describe(c.name, c.steps) {
				out = append(out, "  "+line)
			}
		}
	}
	describeAll(first)
	for _, h := range o.Charts {
		f := files{secrets: map[string]string{}}
		if len(h.Values) > 0 {
			f.values = "<the chart's values>"
		}
		for key, env := range h.ValuesFromEnv {
			f.secrets[key] = "<from $" + env + ">"
		}
		out = append(out, fmt.Sprintf("Helm chart %s %s:", h.Name, h.Version), "  helm "+strings.Join(args(h, o.Kubeconfig, f), " "))
	}
	describeAll(last)
	return out
}

// Secrets lists the variables the add-ons read secrets from: the charts' secret values, and
// the SMB share's credentials.
func Secrets(k config.Kubernetes) []string {
	var out []string
	add := func(env string) {
		if env != "" && !slices.Contains(out, env) {
			out = append(out, env)
		}
	}
	for _, h := range k.Addons.HelmCharts {
		for _, env := range h.ValuesFromEnv {
			add(env)
		}
	}
	if m := k.Storage.SMB; m != nil && slices.Contains(k.Storage.CSI, config.CSISMB) {
		add(m.UsernameEnv)
		add(m.PasswordEnv)
	}
	if slices.Contains(k.Storage.CSI, config.CSIProxmox) {
		add(config.ProxmoxCSITokenEnv)
	}
	slices.Sort(out)
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
