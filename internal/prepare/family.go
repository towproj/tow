package prepare

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Families of systems tow prepares, each with its own tools: apt and dpkg on Ubuntu and
// Debian, dnf and rpm on RHEL and its rebuilds.
const (
	FamilyDebian = "debian"
	FamilyRHEL   = "rhel"
)

// rhelIDs are the RHEL-family systems tow names; any other whose ID_LIKE says rhel is one
// too. CentOS Stream works like them but isn't tested.
var rhelIDs = []string{"rhel", "rocky", "almalinux", "centos"}

// Family says which family /etc/os-release's ID, ID_LIKE and VERSION_ID name: FamilyDebian
// (Ubuntu, Debian and their derivatives), FamilyRHEL (RHEL 9 and 10, Rocky, AlmaLinux), or an
// error saying why tow can't prepare the machine.
func Family(id, idLike, versionID string) (string, error) {
	if DebianFamily(id, idLike) {
		return FamilyDebian, nil
	}
	like := strings.Fields(idLike)
	if slices.Contains(rhelIDs, id) || slices.Contains(like, "rhel") {
		switch major := Major(versionID); {
		case major >= 9:
			return FamilyRHEL, nil
		case major > 0:
			return "", fmt.Errorf("%s %s is older than tow supports: RHEL-family 9 or 10 (its kernel is too old for Kubernetes' network plugins)", id, versionID)
		}
		return "", fmt.Errorf("%s doesn't say its release (VERSION_ID in /etc/os-release)", id)
	}
	if id == "" {
		return "", fmt.Errorf("no /etc/os-release, so tow can't tell which system this is")
	}
	return "", fmt.Errorf("%q isn't a system tow prepares: Ubuntu, Debian, or RHEL 9 or 10 (Rocky, AlmaLinux)", id)
}

// Major is VERSION_ID's major release: 9 for "9.6"; 0 when it has none.
func Major(versionID string) int {
	major, _, _ := strings.Cut(versionID, ".")
	n, _ := strconv.Atoi(major)
	return n
}

// family is the node's family; "" when tow can't prepare it.
func (f Facts) family() string {
	fam, _ := Family(f.OSID, f.OSLike, f.OSVersion)
	return fam
}

// pkg is a package as one family's tools name it.
type pkg struct {
	key  string // its name in Facts.Installed
	spec string // what the package manager installs: the name, for the running kernel when it must be
	why  string
}

// packagesFor are the packages a node of this family and release needs. kernel is the running
// kernel's release (uname -r), for a package built for it; "" = read it on the machine.
func packagesFor(items []Item, family string, major int, kernel string) []pkg {
	var out []pkg
	for _, it := range items {
		switch {
		case it.Only == OnlyDebian && family != FamilyDebian,
			it.Only == OnlyRHEL && family != FamilyRHEL,
			it.Only == OnlyRHEL10 && (family != FamilyRHEL || major < 10):
			continue
		}
		name := it.Name
		if family == FamilyRHEL && it.RPM != "" {
			name = it.RPM
		}
		spec := name
		if it.RunningKernel {
			spec = name + "-" + cmp.Or(kernel, "$(uname -r)")
		}
		out = append(out, pkg{key: name, spec: spec, why: it.Why})
	}
	return out
}

// packageQuery reads which of the packages a node may need are installed, as
// "package:<name>=installed": with dpkg on Debian-family systems, rpm on the others. Both
// families' names are asked: the script runs before the family is known.
func packageQuery(items []Item) string {
	var deb []string
	var rpmLines strings.Builder
	seen := map[string]bool{}
	for _, it := range items {
		if it.Only != OnlyRHEL && it.Only != OnlyRHEL10 && !slices.Contains(deb, it.Name) {
			deb = append(deb, it.Name)
		}
		if it.Only == OnlyDebian {
			continue
		}
		for _, p := range packagesFor([]Item{it}, FamilyRHEL, 99, "") {
			if seen[p.key] {
				continue
			}
			seen[p.key] = true
			fmt.Fprintf(&rpmLines, "  echo \"package:%s=$(rpm -q --quiet %s && echo installed)\"\n", p.key, p.spec)
		}
	}
	if len(deb) == 0 && rpmLines.Len() == 0 {
		return ""
	}
	return "if command -v dpkg-query >/dev/null 2>&1; then\n" +
		fmt.Sprintf("  dpkg-query -W -f='package:${Package}=${db:Status-Status}\\n' %s 2>/dev/null\n", strings.Join(deb, " ")) +
		"elif command -v rpm >/dev/null 2>&1; then\n" + rpmLines.String() + "fi\n"
}

// installPackages installs the missing packages with the family's own package manager, and
// says how to remove them again.
func installPackages(family string, missing []pkg) (script, undo, revert string) {
	var specs, names []string
	for _, p := range missing {
		specs = append(specs, p.spec)
		names = append(names, p.key)
	}
	list := strings.Join(specs, " ")
	if family == FamilyRHEL {
		return "dnf install -y -q " + list, "sudo dnf remove " + list, RemoveRPMs(specs)
	}
	return "export DEBIAN_FRONTEND=noninteractive\n" +
			"apt-get -o DPkg::Lock::Timeout=600 update -qq\n" +
			"apt-get -o DPkg::Lock::Timeout=600 install -y -qq --no-install-recommends " + list,
		"sudo apt-get remove " + strings.Join(names, " "), RemovePackages("remove", names)
}
