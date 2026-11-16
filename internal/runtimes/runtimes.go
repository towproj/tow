// Package runtimes knows the container runtimes kubeadm's machines run: which releases work
// with which Kubernetes, and where the containerd tow installs comes from. k3s brings
// its own containerd and doesn't use this package.
//
// Sources (read 2026-11-10):
//   - containerd's Kubernetes support table and release lines
//     https://github.com/containerd/containerd/blob/main/RELEASES.md
//   - containerd's install steps: release tarball, runc, systemd unit, 'containerd config
//     default'  https://github.com/containerd/containerd/blob/main/docs/getting-started.md
//   - CRI-O follows Kubernetes' minor releases, one package repository per minor on
//     openSUSE's build service  https://github.com/cri-o/packaging
//   - Kubernetes container runtimes: SystemdCgroup, the CRI plugin, the pause image; the
//     kubelet's fallback for containerd 1.x goes in Kubernetes 1.38
//     https://kubernetes.io/docs/setup/production-environment/container-runtimes/
package runtimes

import (
	_ "embed"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The container runtimes a kubeadm cluster can run on.
const (
	Containerd = "containerd"
	CRIO       = "cri-o"
)

// DefaultContainerd is the containerd tow installs when the file names none: the
// long-term support line (2.3, supported until 2028-04-30), which containerd recommends for
// Kubernetes 1.36 and 1.37.
const DefaultContainerd = "2.3.7"

// Runc is the runc installed beside containerd: the one containerd 2.3.7 ships with (its
// release notes).
const Runc = "1.5.2"

// Archs are the architectures containerd and runc publish Linux builds for that tow
// supports, as dpkg names them.
var Archs = []string{"amd64", "arm64"}

// containerdSums are the sha256 of the containerd releases tow pins, by architecture.
// Each matches both the .sha256sum file published beside the tarball and GitHub's own
// digest of it (2026-11-10), so a download is never trusted on one server's word. Other
// releases are checked against the .sha256sum published beside them.
var containerdSums = map[string]map[string]string{
	"2.3.7": {
		"amd64": "09a1b669ac6d21e7e24c3c33a9805b48987ba5845e8d88f039c47013a0c0025c",
		"arm64": "e08530c6c8a23c612606da5d2832aed9bed8fc7f8c1b41df0f148a701b931d69",
	},
}

// runcSums: runc.sha256sum of v1.5.2 and GitHub's digests, which agree (2026-11-10).
var runcSums = map[string]string{
	"amd64": "599f6f94ff8c5057241eff0d54c3c74f95c34935b6457b33fe545defc61e9488",
	"arm64": "d10ecae898361832a059be2089bab92d158aec54661b18ed7346ed79628b46b0",
}

// ContainerdService is containerd's systemd unit, as the containerd repository has it at
// v2.3.7 (sha256 1941362cbaa89dd591b99c32b050d82c583d3cd2e5fa63085d7017457ec5fca8, the
// same git object GitHub serves). It starts /usr/local/bin/containerd, where the release
// tarball puts it; the unit hasn't changed across the 2.x releases.
//
//go:embed containerd.service
var ContainerdService string

// ContainerdURL is a containerd release's Linux tarball.
func ContainerdURL(version, arch string) string {
	v := strings.TrimPrefix(version, "v")
	return fmt.Sprintf("https://github.com/containerd/containerd/releases/download/v%s/containerd-%s-linux-%s.tar.gz", v, v, arch)
}

// ContainerdSum is the sha256 tow pins for a containerd release; "" when it pins none,
// and the .sha256sum published beside the tarball is used instead.
func ContainerdSum(version, arch string) string {
	return containerdSums[strings.TrimPrefix(version, "v")][arch]
}

// RuncURL is runc's binary for an architecture.
func RuncURL(arch string) string {
	return fmt.Sprintf("https://github.com/opencontainers/runc/releases/download/v%s/runc.%s", Runc, arch)
}

// RuncSum is runc's pinned sha256 for an architecture.
func RuncSum(arch string) string { return runcSums[arch] }

// containerdSupport is containerd's own table of the releases it recommends for each
// Kubernetes minor (RELEASES.md, "Kubernetes support", 2026-11-10): the oldest good patch
// of each release line.
var containerdSupport = map[int][]string{
	32: {"2.1.0", "2.0.1", "1.7.24", "1.6.36"},
	33: {"2.1.0", "2.0.4", "1.7.24", "1.6.36"},
	34: {"2.1.3", "2.0.6", "1.7.28", "1.6.39"},
	35: {"2.2.0", "2.1.5", "1.7.28"},
	36: {"2.3.0", "2.2.0"},
	37: {"2.4.0", "2.3.0"},
}

// Recommended lists, for messages, the containerd lines tow accepts for Kubernetes
// 1.<minor>: the table's 2.x ones.
func Recommended(minor int) string {
	var out []string
	for _, v := range containerdSupport[minor] {
		if strings.HasPrefix(v, "2.") {
			out = append(out, v+"+")
		}
	}
	return joinOr(out)
}

// CheckContainerd says whether containerd <version> works for Kubernetes 1.<minor> the way
// tow runs it: 2.0 or newer (tow writes 2.x's config, and the kubelet reads its
// cgroup driver from 2.x), on a line containerd recommends for that minor. minor 0 means
// the Kubernetes release isn't known yet ("stable"): only the first rule applies. A minor
// the table doesn't cover yet takes any 2.x, with a warning.
func CheckContainerd(version string, minor int) (warning string, err error) {
	v, ok := Parse(version)
	if !ok {
		return "", fmt.Errorf("containerd %q isn't a release like %s", version, DefaultContainerd)
	}
	if v.Major < 2 {
		return "", fmt.Errorf("containerd %s is a 1.x release; tow needs 2.0 or newer (the kubelet takes its cgroup driver from containerd 2.x, and drops 1.x in Kubernetes 1.38)", v)
	}
	if minor <= 0 {
		return "", nil
	}
	lines, known := containerdSupport[minor]
	if !known {
		return fmt.Sprintf("containerd's support table doesn't list Kubernetes 1.%d yet; containerd %s is used as asked", minor, v), nil
	}
	for _, l := range lines {
		min, _ := Parse(l)
		if v.Major == min.Major && v.Minor == min.Minor && v.Patch >= min.Patch {
			return "", nil
		}
	}
	return "", fmt.Errorf("containerd %s isn't one containerd recommends for Kubernetes 1.%d (%s, from containerd's RELEASES.md)", v, minor, Recommended(minor))
}

// CheckCRIO says whether CRI-O <version> goes with Kubernetes 1.<minor>: CRI-O follows
// Kubernetes' minor releases, and its packages are published per minor.
func CheckCRIO(version string, minor int) error {
	v, ok := Parse(version)
	if !ok {
		return fmt.Errorf("CRI-O %q isn't a release like 1.37.2", version)
	}
	if minor > 0 && (v.Major != 1 || v.Minor != minor) {
		return fmt.Errorf("CRI-O %s doesn't go with Kubernetes 1.%d: CRI-O follows Kubernetes' minor releases, so it must be a 1.%d release", v, minor, minor)
	}
	return nil
}

// Version is a release: major.minor.patch.
type Version struct{ Major, Minor, Patch int }

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

var release = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`)

// Parse reads a release like 2.3.7, v2.3.7 or 1.7.28-1: a leading v and anything after the
// patch number are ignored.
func Parse(s string) (Version, bool) {
	m := release.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Version{}, false
	}
	var v Version
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	v.Patch, _ = strconv.Atoi(m[3])
	return v, true
}

// KubeMinor is the minor release of a Kubernetes version: 37 for v1.37.2; 0 when it isn't
// one ("stable").
func KubeMinor(version string) int {
	v, ok := Parse(version)
	if !ok || v.Major != 1 {
		return 0
	}
	return v.Minor
}

// joinOr lists choices for a message: "a, b or c".
func joinOr(list []string) string {
	switch len(list) {
	case 0:
		return ""
	case 1:
		return list[0]
	}
	return strings.Join(list[:len(list)-1], ", ") + " or " + list[len(list)-1]
}
