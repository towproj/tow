package addons

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Sources (read 2026-11-12), followed here:
//   - Longhorn: "kubectl -n longhorn-system patch -p '{"value": "true"}' --type=merge lhs
//     deleting-confirmation-flag", then "helm uninstall longhorn -n longhorn-system"
//     https://longhorn.io/docs/1.13.0/deploy/uninstall/
//   - Kubernetes: a StorageClass's parameters, provisioner and reclaim policy can't be updated
//     once it's created  https://kubernetes.io/docs/concepts/storage/storage-classes/

// Builtin is one of the add-ons tow installs itself (not the file's own charts and
// manifests), as a running cluster turns it on, changes it, or turns it off.
type Builtin struct{ c component }

// Builtins are the file's built-in add-ons, in the order they're installed. Each keeps the
// version in o.Running, unless the file pins one.
func Builtins(o Options) []Builtin {
	first, _ := o.components()
	out := make([]Builtin, len(first))
	for i, c := range first {
		out[i] = Builtin{c}
	}
	return out
}

func (b Builtin) Name() string      { return b.c.name }
func (b Builtin) Version() string   { return b.c.version }
func (b Builtin) Pin() string       { return b.c.pin }    // the file's version, "" when it names none
func (b Builtin) Tested() string    { return b.c.tested } // the version tow was tested with
func (b Builtin) CSI() string       { return b.c.csi }    // storage: the file's name for it
func (b Builtin) Class() string     { return b.c.class }  // its StorageClass or IngressClass
func (b Builtin) Namespace() string { return b.c.owns }   // the namespace only it uses, "" when none

// Storage, DefaultClass, ServiceLB and Ingress say what it is: what's checked before it's
// turned off.
func (b Builtin) Storage() bool      { return b.c.role == roleStorage }
func (b Builtin) DefaultClass() bool { return b.c.role == roleDefaultClass }
func (b Builtin) ServiceLB() bool    { return b.c.role == roleServiceLB }
func (b Builtin) Ingress() bool      { return b.c.role == roleIngress }

// Release is its Helm release as namespace/name ("" when it has none): where its version
// is read.
func (b Builtin) Release() string {
	if s := b.c.helmInstall(); s != nil {
		return flag(s.args, "--namespace") + "/" + s.args[2]
	}
	return ""
}

// Deployment is the namespace/name of the Deployment whose image tag is its version, when it
// has no Helm release ("" otherwise).
func (b Builtin) Deployment() string { return b.c.deployment }

// Same reports whether b installs exactly what other does: the same commands, values and
// objects.
func (b Builtin) Same(other Builtin) bool { return sameSteps(b.c.steps, other.c.steps) }

// Objects are the objects it applies itself (not those of its chart or manifest URL), its
// namespace left out: what a change deletes once it no longer applies them.
func (b Builtin) Objects() []Object {
	var out []Object
	for _, d := range b.c.documents() {
		if d.obj.Kind != "Namespace" {
			out = append(out, d.obj)
		}
	}
	return out
}

// Replaced are the StorageClasses old applies that b applies differently: their parameters
// can't change in place, so they're deleted and made again.
func (b Builtin) Replaced(old Builtin) []Object {
	before := map[Object]string{}
	for _, d := range old.c.documents() {
		before[d.obj] = d.text
	}
	var out []Object
	for _, d := range b.c.documents() {
		if text, ok := before[d.obj]; ok && d.obj.Kind == "StorageClass" && text != d.text {
			out = append(out, d.obj)
		}
	}
	return out
}

// Uses reports whether installing or removing it runs program (helm or kubectl).
func (b Builtin) Uses(program string) bool {
	for _, s := range append(slices.Clone(b.c.steps), b.c.removeSteps("")...) {
		if s.program == program {
			return true
		}
	}
	return false
}

// RemoveCommands are what turning it off runs, for the plan's details and a dry run.
func (b Builtin) RemoveCommands() []string {
	return describe(b.c.name, b.c.removeSteps("<kubeconfig>"))
}

// InstallBuiltin installs or changes a built-in add-on, as at creation.
func (o Options) InstallBuiltin(ctx context.Context, b Builtin) error {
	r := runner{helm: o.Helm, kubectl: o.Kubectl, kubeconfig: o.Kubeconfig, dir: o.Dir}
	return r.steps(ctx, b.c.name, b.c.steps)
}

// RemoveBuiltin turns a built-in add-on off: what must come first (Longhorn's confirmation),
// the objects it applied, its release or its manifest, then its own namespace.
func (o Options) RemoveBuiltin(ctx context.Context, b Builtin) error {
	r := runner{helm: o.Helm, kubectl: o.Kubectl, kubeconfig: o.Kubeconfig, dir: o.Dir}
	return r.steps(ctx, b.c.name, b.c.removeSteps(o.Kubeconfig))
}

// removeSteps are what turning c off runs, against kubeconfig.
func (c component) removeSteps(kubeconfig string) []step {
	steps := slices.Clone(c.preRemove)
	var objs []Object
	for _, d := range c.documents() {
		if d.obj.Kind != "Namespace" {
			objs = append(objs, d.obj)
		}
	}
	if len(objs) > 0 {
		steps = append(steps, deleteObjects(kubeconfig, objs))
	}
	if s := c.helmInstall(); s != nil {
		steps = append(steps, step{program: "helm", args: []string{"uninstall", s.args[2], "--namespace", flag(s.args, "--namespace"),
			"--kubeconfig", kubeconfig, "--wait", "--timeout", chartTimeout}})
	}
	for _, s := range c.steps {
		if url := flag(s.args, "-f"); s.program == "kubectl" && s.args[0] == "apply" && strings.HasPrefix(url, "https://") {
			steps = append(steps, step{program: "kubectl", args: []string{"delete", "--ignore-not-found", "--timeout", chartTimeout, "--kubeconfig", kubeconfig, "-f", url}})
		}
	}
	steps = append(steps, c.postRemove...)
	if c.owns != "" {
		steps = append(steps, step{program: "kubectl", args: []string{"delete", "namespace", c.owns, "--ignore-not-found", "--timeout", chartTimeout, "--kubeconfig", kubeconfig}})
	}
	return steps
}

// helmInstall is c's helm upgrade --install, nil when it has none.
func (c component) helmInstall() *step {
	for i, s := range c.steps {
		if s.program == "helm" && len(s.args) > 2 && s.args[0] == "upgrade" {
			return &c.steps[i]
		}
	}
	return nil
}

// flag is the value after name in args, "" when it isn't there.
func flag(args []string, name string) string {
	if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// document is one object a component applies from stdin, and its text.
type document struct {
	obj  Object
	text string
}

// documents are the objects c applies from stdin (kubectl apply -f -), in order.
func (c component) documents() []document {
	var out []document
	for _, s := range c.steps {
		if s.program != "kubectl" || s.args[0] != "apply" || s.stdin == "" {
			continue
		}
		dec := yaml.NewDecoder(strings.NewReader(s.stdin))
		for {
			var n yaml.Node
			if err := dec.Decode(&n); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				panic(fmt.Sprintf("%s: tow's own YAML doesn't parse: %v", c.name, err)) // a bug, caught by the tests
			}
			var m struct {
				APIVersion string `yaml:"apiVersion"`
				Kind       string `yaml:"kind"`
				Metadata   struct{ Name, Namespace string }
			}
			if err := n.Decode(&m); err != nil || m.Kind == "" {
				continue
			}
			var text bytes.Buffer
			yaml.NewEncoder(&text).Encode(&n)
			out = append(out, document{Object{APIVersion: m.APIVersion, Kind: m.Kind, Namespace: m.Metadata.Namespace, Name: m.Metadata.Name}, text.String()})
		}
	}
	return out
}

// sameSteps reports whether two lists of steps run the same commands with the same input.
func sameSteps(a, b []step) bool { return reflect.DeepEqual(a, b) }

// deleteObjects is kubectl delete of objects by name; one already gone is fine.
func deleteObjects(kubeconfig string, objs []Object) step {
	var b strings.Builder
	for _, obj := range objs {
		fmt.Fprintf(&b, "---\napiVersion: %s\nkind: %s\nmetadata:\n  name: %q\n", obj.APIVersion, obj.Kind, obj.Name)
		if obj.Namespace != "" {
			fmt.Fprintf(&b, "  namespace: %q\n", obj.Namespace)
		}
	}
	return step{program: "kubectl", args: []string{"delete", "--ignore-not-found", "--timeout", chartTimeout, "--kubeconfig", kubeconfig, "-f", "-"}, stdin: b.String()}
}

// chartVersion is the version in what helm list says a release runs: 1.13.0 for
// "longhorn-1.13.0", 4.13.6 for "csi-driver-nfs-v4.13.6".
func chartVersion(chart string) string {
	for i := 0; i+1 < len(chart); i++ {
		if chart[i] != '-' {
			continue
		}
		rest := strings.TrimPrefix(chart[i+1:], "v")
		if rest != "" && rest[0] >= '0' && rest[0] <= '9' {
			return rest
		}
	}
	return ""
}

// ImageVersion is an image's tag without its v: 0.0.37 for
// docker.io/rancher/local-path-provisioner:v0.0.37.
func ImageVersion(image string) string {
	image, _, _ = strings.Cut(image, "@")
	if i := strings.LastIndex(image, ":"); i >= 0 && !strings.Contains(image[i:], "/") {
		return strings.TrimPrefix(image[i+1:], "v")
	}
	return ""
}

// Older reports whether version a comes before b, both dotted numbers (a suffix like -rc1
// is ignored); false when either isn't one.
func Older(a, b string) bool {
	pa, pb := numbers(a), numbers(b)
	if pa == nil || pb == nil {
		return false
	}
	return slices.Compare(pa, pb) < 0
}

func numbers(v string) []int {
	v, _, _ = strings.Cut(strings.TrimPrefix(v, "v"), "-")
	var out []int
	for _, p := range strings.Split(v, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}
