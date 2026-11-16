package prepare

import (
	"fmt"
	"strings"
)

// The API name: on clouds where a machine behind a load balancer can't reach the load
// balancer's own address (GCP routes it back to the machine itself; Azure drops it when it
// lands there), the address in front of the control planes is a name instead, which each
// machine resolves in its /etc/hosts: a worker to the load balancer, a control plane to
// itself once its API server runs, to the first control plane while it joins.

// apiNameMark ends tow's line, so it's found again and only it is replaced or removed.
const apiNameMark = "# tow API name"

// HostsScript points name at ip in /etc/hosts: tow's line for it is replaced, or added.
// The machine's other lines stay as they are.
func HostsScript(name, ip string) string {
	return fmt.Sprintf("sed -i '/[[:space:]]%s[[:space:]]*%s$/d' /etc/hosts\nprintf '%%s %%s %%s\\n' '%s' '%s' '%s' >> /etc/hosts\n",
		sedEscape(name), apiNameMark, ip, name, apiNameMark)
}

// HostsRevert removes tow's line for name; nothing when it's gone already.
func HostsRevert(name string) string {
	return fmt.Sprintf("if [ -f /etc/hosts ]; then sed -i '/[[:space:]]%s[[:space:]]*%s$/d' /etc/hosts; fi\n", sedEscape(name), apiNameMark)
}

// sedEscape makes a name match itself in a sed pattern: its dots are literal.
func sedEscape(name string) string { return strings.ReplaceAll(name, ".", `\.`) }
