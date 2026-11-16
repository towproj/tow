package k3s

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"

	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/remote"
	"gopkg.in/yaml.v3"
)

// Where k3s keeps what tow reads and writes on a machine (k3s docs: configuration, token).
const (
	ConfigFile     = "/etc/rancher/k3s/config.yaml"            // every setting, root-only (0600)
	kubeconfigFile = "/etc/rancher/k3s/k3s.yaml"               // the admin kubeconfig, root-only
	tokenFile      = "/var/lib/rancher/k3s/server/token"       // K10<CA hash>::server:<password>
	agentTokenFile = "/var/lib/rancher/k3s/server/agent-token" // K10<CA hash>::node:<password>
	apiPort        = "6443"

	// The first line of every config.yaml tow writes: it tells a machine tow set up
	// from one where someone else installed k3s.
	markerPrefix = "# written by tow for cluster "
	markerSuffix = "; tow rewrites this file"

	roleServer = "control plane"
	roleAgent  = "worker"
)

// Release is the k3s release a version means: a plain v1.37.2 is its first k3s build,
// v1.37.2+k3s1, as k3d's images are.
func Release(version string) string {
	if !strings.Contains(version, "+k3s") {
		version += "+k3s1"
	}
	return version
}

// installerURL is the official install script as it was at the release itself, so the script
// doesn't change under a pinned version. It verifies the k3s binary's sha256 sum.
func installerURL(release string) string {
	return "https://raw.githubusercontent.com/k3s-io/k3s/" + strings.ReplaceAll(release, "+", "%2B") + "/install.sh"
}

// newToken is a join password: k3s's first server takes only a plain one (k3s docs: token).
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func marker(cluster, role string) string {
	return markerPrefix + cluster + " (" + role + ")" + markerSuffix
}

// owner reads the marker of a config.yaml: which cluster and role it was written for. ok is
// false when tow didn't write it.
func owner(config []byte) (cluster, role string, ok bool) {
	first, _, _ := bytes.Cut(config, []byte("\n"))
	rest, found := strings.CutPrefix(string(first), markerPrefix)
	if !found {
		return "", "", false
	}
	rest = strings.TrimSuffix(rest, markerSuffix)
	i := strings.LastIndex(rest, " (")
	if i < 0 || !strings.HasSuffix(rest, ")") {
		return "", "", false
	}
	return rest[:i], rest[i+2 : len(rest)-1], true
}

// tokensIn reads the join passwords a config.yaml tow wrote holds.
func tokensIn(config []byte) (server, agent string) {
	var m map[string]any
	if yaml.Unmarshal(config, &m) != nil {
		return "", ""
	}
	server, _ = m["token"].(string)
	agent, _ = m["agent-token"].(string)
	return server, agent
}

// setup is what one machine's config.yaml says.
type setup struct {
	cluster, role string
	name, ip      string
	first         bool   // the control plane that starts the cluster
	server        string // the others: the address they join
	token         string
	agentToken    string   // control planes: the workers' password
	sans          []string // control planes: extra names for the API's certificate
	flags         []providers.Flag
	// selinux: the machine enforces SELinux, so k3s must too (k3s docs, SELinux support:
	// the installer adds the policy, the selinux setting turns it on in k3s's containerd).
	selinux bool
}

type entry struct {
	key    string
	values []string
	list   bool
}

// alwaysLists are repeatable flags, written as YAML lists even with one value.
func alwaysList(key string) bool {
	return strings.HasSuffix(key, "-arg") || slices.Contains([]string{"tls-san", "node-label", "node-taint", "disable"}, key)
}

// renderConfig writes a machine's config.yaml: tow's own keys first, then the file's
// settings. Repeated flags become YAML lists (k3s docs: configuration). value turns a flag
// into its value; it reads a secret from its variable.
func renderConfig(s setup, value func(providers.Flag) string) ([]byte, error) {
	var entries []entry
	add := func(key, v string) {
		for i := range entries {
			if entries[i].key == key {
				if key == "tls-san" && slices.Contains(entries[i].values, v) {
					return
				}
				entries[i].values = append(entries[i].values, v)
				entries[i].list = true
				return
			}
		}
		entries = append(entries, entry{key: key, values: []string{v}, list: alwaysList(key)})
	}

	if s.first {
		add("cluster-init", "true")
	}
	if s.server != "" {
		add("server", s.server)
	}
	if s.token != "" {
		add("token", s.token)
	}
	if s.agentToken != "" {
		add("agent-token", s.agentToken)
	}
	add("node-name", s.name)
	if s.ip != "" {
		add("node-ip", s.ip)
	}
	if s.selinux && !slices.ContainsFunc(s.flags, func(f providers.Flag) bool { return f.Name == "selinux" }) {
		add("selinux", "true")
	}
	// The file's certSANs first, then the addresses tow adds.
	for _, f := range s.flags {
		if f.Name == "tls-san" {
			add("tls-san", value(f))
		}
	}
	for _, san := range s.sans {
		add("tls-san", san)
	}
	for _, f := range s.flags {
		if f.Name != "tls-san" {
			add(f.Name, value(f))
		}
	}

	doc := &yaml.Node{Kind: yaml.MappingNode}
	for _, e := range entries {
		key := &yaml.Node{Kind: yaml.ScalarNode, Value: e.key}
		var v *yaml.Node
		if e.list {
			v = &yaml.Node{Kind: yaml.SequenceNode}
			for _, x := range e.values {
				v.Content = append(v.Content, scalar(x))
			}
		} else {
			v = scalar(e.values[0])
		}
		doc.Content = append(doc.Content, key, v)
	}
	var b bytes.Buffer
	b.WriteString(marker(s.cluster, s.role) + "\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// scalar writes on/off flags as YAML booleans (k3s docs: configuration) and everything else as
// a string, quoted when YAML would read it as something else (10, null). YAML 1.1 readers
// also take yes/no/on/off for booleans, so those are always quoted.
func scalar(v string) *yaml.Node {
	if v == "true" || v == "false" {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: v}
	}
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
	if oldBool.MatchString(v) {
		n.Style = yaml.DoubleQuotedStyle
	}
	return n
}

var oldBool = regexp.MustCompile(`^(?i:y|yes|n|no|on|off)$`)

// hostOf is the host part of host:port, or the address itself.
func hostOf(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// asRoot runs the script on stdin as root (remote.AsRoot).
const asRoot = remote.AsRoot

// checkScript reads, changing nothing, what's already on a machine.
const checkScript = `k=$(command -v k3s 2>/dev/null || true)
[ -z "$k" ] && [ -x /usr/local/bin/k3s ] && k=/usr/local/bin/k3s
if [ -n "$k" ] || [ -e /etc/systemd/system/k3s.service ] || [ -e /etc/systemd/system/k3s-agent.service ]; then echo installed=yes; else echo installed=no; fi
v=""
[ -n "$k" ] && v=$("$k" --version 2>/dev/null | head -n1 | cut -d' ' -f3)
echo "version=$v"
echo "k3s=$(systemctl is-active k3s 2>/dev/null)"
echo "k3s-agent=$(systemctl is-active k3s-agent 2>/dev/null)"
echo "config=$(base64 -w0 ` + ConfigFile + ` 2>/dev/null)"
echo "record=$(base64 -w0 /etc/tow/prepared.json 2>/dev/null)"
echo "selinux=$(getenforce 2>/dev/null)"
echo complete=yes
`

const heredocEnd = "TOW_EOF"

// installScript writes the machine's config.yaml (root-only, through stdin: its passwords
// never reach a command line), then runs the release's installer, which downloads k3s,
// checks its sha256 sum and (re)starts the service. The installer skips the restart when
// k3s's binary and service didn't change (its "No change detected"), so a new config.yaml
// alone would wait for the next restart: INSTALL_K3S_FORCE_RESTART restarts it. Then the
// install is recorded.
func installScript(config []byte, release, exec, record string) (string, error) {
	if bytes.Contains(config, []byte(heredocEnd)) {
		return "", fmt.Errorf("config.yaml contains the heredoc marker %s", heredocEnd)
	}
	return `set -e
mkdir -p /etc/rancher/k3s
(umask 077; cat > ` + ConfigFile + `.tow <<'` + heredocEnd + `'
` + string(config) + heredocEnd + `
)
mv -f ` + ConfigFile + `.tow ` + ConfigFile + `
installer=$(mktemp)
trap 'rm -f "$installer"' EXIT
curl -fsSL --retry 3 --retry-delay 2 -o "$installer" '` + installerURL(release) + `'
INSTALL_K3S_VERSION='` + release + `' INSTALL_K3S_EXEC='` + exec + `' INSTALL_K3S_FORCE_RESTART=true sh "$installer"
` + record, nil
}
