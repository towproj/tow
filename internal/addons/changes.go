package addons

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/towproj/tow/internal/config"
)

// Object is one object a manifest created, as state records it: what deletes it when the
// manifest drops it, or leaves the file.
type Object struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
}

func (o Object) String() string {
	s := strings.ToLower(o.Kind) + "/" + o.Name
	if o.Namespace != "" {
		s += " in " + o.Namespace
	}
	return s
}

// objectsIn reads the objects kubectl printed with -o json: one object, a List, or several
// of either one after the other.
func objectsIn(out string) ([]Object, error) {
	type meta struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
		} `json:"metadata"`
		Items []json.RawMessage `json:"items"`
	}
	var objs []Object
	var add func(raw json.RawMessage) error
	add = func(raw json.RawMessage) error {
		var m meta
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		if m.Kind == "List" || strings.HasSuffix(m.Kind, "List") && m.Items != nil {
			for _, item := range m.Items {
				if err := add(item); err != nil {
					return err
				}
			}
			return nil
		}
		objs = append(objs, Object{APIVersion: m.APIVersion, Kind: m.Kind, Namespace: m.Metadata.Namespace, Name: m.Metadata.Name})
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(out))
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("read kubectl's output: %w", err)
		}
		if err := add(raw); err != nil {
			return nil, err
		}
	}
	return objs, nil
}

// Missing lists the objects of had that now doesn't have: what a manifest dropped.
func Missing(had, now []Object) []Object {
	var out []Object
	for _, o := range had {
		if !slices.Contains(now, o) {
			out = append(out, o)
		}
	}
	return out
}

// InstallChart installs or upgrades one of the file's Helm charts, as at creation.
func (o Options) InstallChart(ctx context.Context, h config.HelmChart) error {
	return o.install(ctx, h)
}

// UninstallChart removes a release tow installed, and waits for its objects to go. Its
// namespace stays, and so do the volumes its StatefulSets claimed (Helm leaves them).
func (o Options) UninstallChart(ctx context.Context, name, namespace string) error {
	out, errOut, err := run(ctx, o.Helm, []string{"uninstall", name, "--namespace", namespace, "--kubeconfig", o.Kubeconfig, "--wait", "--timeout", chartTimeout}, "")
	if err != nil {
		return fmt.Errorf("helm uninstall %s: %w\n%s", name, err, strings.TrimSpace(out+"\n"+errOut))
	}
	return nil
}

// Releases lists the cluster's Helm releases, as namespace/name, whatever their status.
// Each status is named: Helm 4 dropped list's --all, and Helm 3 shows only deployed and
// failed ones without it.
func (o Options) Releases(ctx context.Context) ([]string, error) {
	versions, err := o.ReleaseVersions(ctx)
	if err != nil {
		return nil, err
	}
	ids := slices.Collect(maps.Keys(versions))
	slices.Sort(ids)
	return ids, nil
}

// ReleaseVersions are the cluster's Helm releases (namespace/name) and the chart version
// each one runs.
func (o Options) ReleaseVersions(ctx context.Context) (map[string]string, error) {
	out, errOut, err := run(ctx, o.Helm, []string{"list", "--all-namespaces", "--deployed", "--failed", "--pending", "--superseded",
		"--uninstalling", "--output", "json", "--kubeconfig", o.Kubeconfig}, "")
	if err != nil {
		return nil, fmt.Errorf("helm list: %w\n%s", err, strings.TrimSpace(errOut))
	}
	var list []struct{ Name, Namespace, Chart string }
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("read helm list: %w", err)
	}
	versions := map[string]string{}
	for _, r := range list {
		versions[r.Namespace+"/"+r.Name] = chartVersion(r.Chart)
	}
	return versions, nil
}

// manifestApply is the manifest's apply step (its namespace's first), with extra flags.
func (o Options) manifestApply(m config.Manifest, extra ...string) []step {
	steps := o.manifest(m).steps
	last := &steps[len(steps)-1]
	last.args = append(slices.Clone(last.args), extra...)
	return steps
}

// CheckManifest tells, changing nothing, whether a manifest differs from what runs, and the
// objects it would make: kubectl diff, then a server-side dry run. While its namespace
// doesn't exist, it differs and its objects aren't known (nil) until it's applied.
func (o Options) CheckManifest(ctx context.Context, m config.Manifest) (differs bool, objs []Object, err error) {
	r := runner{helm: o.Helm, kubectl: o.Kubectl, kubeconfig: o.Kubeconfig, dir: o.Dir}
	apply := o.manifestApply(m)
	last := apply[len(apply)-1]

	diff := last
	diff.args = append([]string{"diff"}, last.args[1:]...) // kubectl diff takes apply's flags
	out, errOut, err := r.step(ctx, "manifest "+m.Path, diff, "")
	switch code := exitCode(err); {
	case err == nil:
	case code == 1: // kubectl diff: the manifest differs from what runs
		differs = true
	case strings.Contains(errOut, "not found") && strings.Contains(errOut, "namespaces"):
		differs = true // its namespace doesn't exist yet: everything in it is new
	default:
		return false, nil, fmt.Errorf("manifest %s: kubectl diff: %w\n%s", m.Path, err, strings.TrimSpace(errOut+"\n"+out))
	}

	dry := last
	dry.args = append(slices.Clone(last.args), "--dry-run=server", "--output", "json")
	out, errOut, err = r.step(ctx, "manifest "+m.Path, dry, "")
	if err != nil {
		if strings.Contains(errOut, "not found") && strings.Contains(errOut, "namespaces") {
			return true, nil, nil // nothing to compare with until its namespace exists
		}
		return false, nil, fmt.Errorf("manifest %s: kubectl apply --dry-run: %w\n%s", m.Path, err, strings.TrimSpace(errOut+"\n"+out))
	}
	objs, err = objectsIn(out)
	return differs, objs, err
}

// ApplyManifest applies one of the file's manifests as at creation, and returns the objects
// it made.
func (o Options) ApplyManifest(ctx context.Context, m config.Manifest) ([]Object, error) {
	r := runner{helm: o.Helm, kubectl: o.Kubectl, kubeconfig: o.Kubeconfig, dir: o.Dir}
	var objs []Object
	steps := o.manifestApply(m, "--output", "json")
	for i, s := range steps {
		out, errOut, err := r.step(ctx, "manifest "+m.Path, s, "")
		if err != nil {
			return nil, fmt.Errorf("manifest %s: %s %s: %w\n%s", m.Path, s.program, s.args[0], err, strings.TrimSpace(errOut+"\n"+out))
		}
		if i == len(steps)-1 {
			if objs, err = objectsIn(out); err != nil {
				return nil, err
			}
		}
	}
	return objs, nil
}

// DeleteObjects deletes objects a manifest made; one already gone is fine.
func (o Options) DeleteObjects(ctx context.Context, objs []Object) error {
	if len(objs) == 0 {
		return nil
	}
	s := deleteObjects(o.Kubeconfig, objs)
	out, errOut, err := run(ctx, o.Kubectl, s.args, s.stdin)
	if err != nil {
		return fmt.Errorf("kubectl delete: %w\n%s", err, strings.TrimSpace(errOut+"\n"+out))
	}
	return nil
}

// exitCode is a program's exit code (an *exec.ExitError has one), or -1.
func exitCode(err error) int {
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}
