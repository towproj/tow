package runtimes

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestCheckContainerd(t *testing.T) {
	tests := []struct {
		version string
		minor   int
		err     string // "" = accepted
		warn    bool
	}{
		{"2.3.7", 37, "", false},
		{"v2.4.2", 37, "", false},
		{"2.3.0", 36, "", false},
		{"2.2.10", 36, "", false},
		{"2.2.10", 37, "isn't one containerd recommends for Kubernetes 1.37 (2.4.0+ or 2.3.0+", false},
		{"2.1.4", 35, "(2.2.0+ or 2.1.5+", false}, // below the line's oldest good patch
		{"2.1.5", 35, "", false},
		{"1.7.28", 35, "is a 1.x release; tow needs 2.0 or newer", false}, // listed, but 1.x
		{"1.7.28", 0, "is a 1.x release", false},
		{"2.0.0", 0, "", false}, // "stable": only 2.x is required
		{"2.4.2", 38, "", true}, // not in the table yet: accepted, with a warning
		{"latest", 37, "isn't a release like 2.3.7", false},
	}
	for _, tt := range tests {
		warn, err := CheckContainerd(tt.version, tt.minor)
		if tt.err == "" && err != nil || tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)) {
			t.Errorf("CheckContainerd(%s, 1.%d) = %v, want %q", tt.version, tt.minor, err, tt.err)
		}
		if (warn != "") != tt.warn {
			t.Errorf("CheckContainerd(%s, 1.%d) warning = %q", tt.version, tt.minor, warn)
		}
	}
}

func TestDefaultContainerdFitsTheTestedKubernetes(t *testing.T) {
	for _, minor := range []int{36, 37} {
		if warn, err := CheckContainerd(DefaultContainerd, minor); err != nil || warn != "" {
			t.Errorf("default containerd %s with Kubernetes 1.%d: %v %q", DefaultContainerd, minor, err, warn)
		}
	}
	for _, arch := range Archs {
		if len(ContainerdSum(DefaultContainerd, arch)) != 64 || len(RuncSum(arch)) != 64 {
			t.Errorf("%s: missing a pinned sha256", arch)
		}
	}
	if ContainerdSum("2.4.2", "amd64") != "" {
		t.Error("only pinned releases have a sum in tow")
	}
}

func TestCheckCRIO(t *testing.T) {
	if err := CheckCRIO("1.37.2", 37); err != nil {
		t.Error(err)
	}
	if err := CheckCRIO("v1.37.0", 0); err != nil {
		t.Error(err)
	}
	if err := CheckCRIO("1.36.4", 37); err == nil || !strings.Contains(err.Error(), "must be a 1.37 release") {
		t.Errorf("err = %v", err)
	}
}

func TestURLs(t *testing.T) {
	if got := ContainerdURL("v2.3.7", "arm64"); got != "https://github.com/containerd/containerd/releases/download/v2.3.7/containerd-2.3.7-linux-arm64.tar.gz" {
		t.Error(got)
	}
	if got := RuncURL("amd64"); got != "https://github.com/opencontainers/runc/releases/download/v1.5.2/runc.amd64" {
		t.Error(got)
	}
}

// The embedded unit is containerd's own, byte for byte.
func TestContainerdServiceIsUpstreams(t *testing.T) {
	sum := sha256.Sum256([]byte(ContainerdService))
	if got := hex.EncodeToString(sum[:]); got != "1941362cbaa89dd591b99c32b050d82c583d3cd2e5fa63085d7017457ec5fca8" {
		t.Errorf("containerd.service changed: %s", got)
	}
}

func TestParse(t *testing.T) {
	for in, want := range map[string]string{"2.3.7": "2.3.7", "v1.37.2": "1.37.2", "1.7.28-1": "1.7.28", "v1.37.2+k3s1": "1.37.2"} {
		if v, ok := Parse(in); !ok || v.String() != want {
			t.Errorf("Parse(%q) = %v, %v", in, v, ok)
		}
	}
	if KubeMinor("v1.37.2") != 37 || KubeMinor("stable") != 0 {
		t.Error("KubeMinor")
	}
}
