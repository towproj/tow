package k3d

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/towproj/tow/internal/kubeapi"
	"github.com/towproj/tow/internal/kubecfg"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/secretenv"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/ui"
)

const createTimeout = 5 * time.Minute

type Provider struct{}

func New() providers.Provider { return &Provider{} }

func (p *Provider) Name() string { return "k3d" }

func (p *Provider) PreFlight(ctx context.Context, spec providers.Spec) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker not found: required for k3d provisioning")
	}
	if _, err := exec.LookPath("k3d"); err != nil {
		// Avoid forcing users to search docs for installation.
		return fmt.Errorf("k3d not found: install via https://k3d.io/#installation")
	}
	return nil
}

// Plan shows the k3d command; a secret shows as the variable it comes from ($NAME), unread.
func (p *Provider) Plan(ctx context.Context, spec providers.Spec) ([]string, error) {
	steps := []string{fmt.Sprintf("Would run: k3d %s", strings.Join(createArgs(spec, nil), " "))}
	if k := spec.Kubernetes; k != nil {
		if k.Merge {
			if err := kubecfg.CheckFree(k.Context); err != nil {
				return nil, err
			}
			steps = append(steps, fmt.Sprintf("Would add context '%s' to your kubeconfig and switch to it", k.Context))
		}
		steps = append(steps, fmt.Sprintf("Would save the cluster's kubeconfig to %s", kubeconfigPath(spec.Name)))
		if len(k.ServerRoleLabels)+len(k.AgentRoleLabels) > 0 {
			steps = append(steps, "Would add the role labels through the API once the nodes registered")
		}
	}
	return steps, nil
}

// Provision returns no nodes: k3d manages them itself. tow writes the kubeconfig under
// the context the file names; without a Kubernetes spec, k3d's own (k3d-<name>) stays.
func (p *Provider) Provision(ctx context.Context, spec providers.Spec) (*providers.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()

	// Secrets are read (and hidden from all output) before anything is created.
	secrets, err := readSecrets(spec)
	if err != nil {
		return nil, err
	}
	k := spec.Kubernetes
	if k != nil && k.Merge {
		if err := kubecfg.CheckFree(k.Context); err != nil {
			return nil, err
		}
	}

	cmd := command(ctx, "k3d", createArgs(spec, secrets)...)
	ui.Debug("exec: %s %s", cmd.Path, shown(cmd.Args, secrets))

	var stderr bytes.Buffer
	out := ui.NewRedactingWriter(os.Stdout)
	cmd.Stdout = out
	cmd.Stderr = &stderr

	err = cmd.Run()
	out.Flush()
	if err != nil {
		// Surface provider stderr without duplicating the live console stream.
		return nil, fmt.Errorf("cluster creation failed: %w\n%s", err, strings.TrimSpace(stderr.String()))
	}
	if k == nil {
		return &providers.Result{KubeContext: KubeContext(spec.Name)}, nil
	}

	data, err := writeKubeconfig(ctx, spec)
	if err == nil {
		err = roleLabels(ctx, spec, data)
	}
	if err != nil {
		// A cluster nobody can reach, or not as the file describes, is no use: remove it
		// rather than leave it behind.
		_ = command(context.Background(), "k3d", "cluster", "delete", spec.Name).Run()
		_ = os.Remove(kubeconfigPath(spec.Name))
		if k.Merge {
			_ = kubecfg.Remove(k.Context)
		}
		return nil, fmt.Errorf("cluster created but not finished, so it was removed again: %w", err)
	}
	return &providers.Result{KubeContext: k.Context, Kubeconfig: kubeconfigPath(spec.Name)}, nil
}

// KubeContext is the kubeconfig context k3d creates: the cluster name with a "k3d-" prefix.
func KubeContext(name string) string { return "k3d-" + name }

func kubeconfigPath(name string) string { return filepath.Join(state.Dir(name), "kubeconfig") }

// writeKubeconfig keeps the cluster's kubeconfig in its state folder (owner-only), named after
// the file's context, and merges it into the user's unless the file says not to. The
// kubeconfig holds the admin key: it is never printed.
func writeKubeconfig(ctx context.Context, spec providers.Spec) ([]byte, error) {
	cmd := command(ctx, "k3d", "kubeconfig", "get", spec.Name)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("k3d kubeconfig get: %w\n%s", err, strings.TrimSpace(stderr.String()))
	}
	data := stdout.Bytes()
	return data, kubecfg.Store(data, spec.Kubernetes.Context, kubeconfigPath(spec.Name), spec.Kubernetes.Merge)
}

// roleLabels adds the role labels (node-role.kubernetes.io/...) through the API: a kubelet
// may not set them on its own node. Servers are the nodes k3s marks as control planes.
func roleLabels(ctx context.Context, spec providers.Spec, kubeconfig []byte) error {
	k := spec.Kubernetes
	if len(k.ServerRoleLabels)+len(k.AgentRoleLabels) == 0 {
		return nil
	}
	api, err := kubeapi.New(kubeconfig)
	if err != nil {
		return err
	}
	want := 1
	if l := spec.Local; l != nil {
		want = max(l.Servers, 1) + l.Agents
	}
	nodes, err := api.WaitCount(ctx, want)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		labels := k.AgentRoleLabels
		if n.ControlPlane {
			labels = k.ServerRoleLabels
		}
		if err := api.Label(ctx, n.Name, labels); err != nil {
			return err
		}
	}
	return nil
}

// readSecrets reads the variables the k3s flags take their values from.
func readSecrets(spec providers.Spec) (map[string]string, error) {
	secrets := map[string]string{}
	if k := spec.Kubernetes; k != nil {
		for _, f := range append(append([]providers.Flag{}, k.ServerFlags...), k.AgentFlags...) {
			if f.Env == "" || secrets[f.Env] != "" {
				continue
			}
			v, err := secretenv.Read(f.Env)
			if err != nil {
				return nil, err
			}
			secrets[f.Env] = v
		}
	}
	return secrets, nil
}

// createArgs builds k3d's command line. secrets holds the values of flags read from
// environment variables; without them (dry run) each shows as $VARIABLE.
func createArgs(spec providers.Spec, secrets map[string]string) []string {
	args := []string{"cluster", "create", spec.Name}
	// k3d's defaults are 1 server and 0 agents, so the flags only appear when the file asks
	// for more.
	agents := 0
	if l := spec.Local; l != nil {
		if l.Servers > 1 {
			args = append(args, "--servers", strconv.Itoa(l.Servers))
		}
		if l.Agents > 0 {
			args = append(args, "--agents", strconv.Itoa(l.Agents))
		}
		agents = l.Agents
	}
	if spec.Version != "" {
		args = append(args, "--image", image(spec.Version))
	}
	if k := spec.Kubernetes; k != nil {
		for _, f := range k.ServerFlags {
			args = append(args, "--k3s-arg", k3sArg(f, "server:*", secrets))
		}
		if agents > 0 {
			for _, f := range k.AgentFlags {
				args = append(args, "--k3s-arg", k3sArg(f, "agent:*", secrets))
			}
		}
		// tow writes the kubeconfig itself, under the context name the file chose.
		args = append(args, "--kubeconfig-update-default=false", "--kubeconfig-switch-context=false")
	}
	return args
}

// k3sArg writes one k3s flag for k3d: --name=value@nodefilter, with a literal '@' escaped.
// A secret's value comes from secrets, or shows as $VARIABLE when they weren't read. On a
// local cluster 'docker inspect' shows it (config warns); tow's own output never does.
func k3sArg(f providers.Flag, filter string, secrets map[string]string) string {
	value := f.Value
	if f.Env != "" {
		secret, ok := secrets[f.Env]
		if !ok {
			secret = "$" + f.Env
		}
		value += secret
	}
	return "--" + f.Name + "=" + strings.ReplaceAll(value, "@", `\@`) + "@" + filter
}

// shown is k3d's command line for --verbose, each secret's value replaced by its variable's
// name, whatever its length: the output's own redaction skips short values, which could be
// ordinary words. Longest first, so a secret inside another doesn't leave the rest showing.
func shown(args []string, secrets map[string]string) string {
	s := strings.Join(args, " ")
	names := slices.SortedFunc(maps.Keys(secrets), func(a, b string) int { return len(secrets[b]) - len(secrets[a]) })
	for _, name := range names {
		v := secrets[name]
		if v == "" {
			continue
		}
		for _, form := range []string{strings.ReplaceAll(v, "@", `\@`), v} { // k3sArg escapes @
			s = strings.ReplaceAll(s, form, "<$"+name+">")
		}
	}
	return s
}

// command starts k3d or docker without the variables that hold the file's secrets.
func command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = secretenv.Environ()
	return cmd
}

// image maps a k3s release (v1.36.5+k3s1) to its Docker tag (v1.36.5-k3s1).
// A plain Kubernetes version gets the first k3s build of that release.
func image(version string) string {
	if !strings.Contains(version, "+k3s") {
		version += "+k3s1"
	}
	return "rancher/k3s:" + strings.Replace(version, "+", "-", 1)
}

func (p *Provider) Status(ctx context.Context, spec providers.Spec) (providers.StatusResult, error) {
	listCmd := command(ctx, "k3d", "cluster", "list")
	ui.Debug("exec: %s", listCmd.String())

	var listOut bytes.Buffer
	listCmd.Stdout = &listOut
	if err := listCmd.Run(); err != nil {
		return providers.StatusResult{Healthy: false, State: "error", Message: "k3d daemon unreachable"}, nil
	}

	scanner := bufio.NewScanner(&listOut)
	found := false
	res := providers.StatusResult{Healthy: false}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "NAME") && strings.Contains(line, spec.Name) {
			fields := strings.Fields(line)
			if len(fields) >= 4 {
				res.ControlPlanes = fields[1]
				res.Workers = fields[2]
				found = true
			}
			break
		}
	}
	if !found {
		res.State = "not_found"
		return res, nil
	}

	serverName := fmt.Sprintf("k3d-%s-server-0", spec.Name)
	dockerCmd := command(ctx, "docker", "ps", "--filter", fmt.Sprintf("name=%s", serverName), "--format", "{{.Status}}")
	ui.Debug("exec: %s", dockerCmd.String())

	var dockerOut bytes.Buffer
	dockerCmd.Stdout = &dockerOut
	if err := dockerCmd.Run(); err != nil {
		res.State = "error"
		res.Message = "docker check failed"
		return res, nil
	}

	status := strings.TrimSpace(dockerOut.String())
	if strings.Contains(status, "Up") {
		res.Healthy = true
		res.State = "ready"
	} else {
		res.State = "stopped"
		res.Message = "control-plane container not running"
	}
	return res, nil
}

// Teardown deletes the cluster, then the context tow added to the user's kubeconfig
// (k3d removes only its own). The copy in the state folder goes with the state.
func (p *Provider) Teardown(ctx context.Context, spec providers.Spec) error {
	cmd := command(ctx, "k3d", "cluster", "delete", spec.Name)
	ui.Debug("exec: %s", cmd.String())

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cluster deletion failed: %w\n%s", err, strings.TrimSpace(stderr.String()))
	}
	if k := spec.Kubernetes; k != nil && k.Merge {
		if err := kubecfg.Remove(k.Context); err != nil {
			return fmt.Errorf("cluster deleted, but its kubeconfig context '%s' remains: %w", k.Context, err)
		}
	}
	return nil
}
