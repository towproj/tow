package config

// Minimum is the smallest machine a distribution accepts for a role.
//
// CPU and memory are the distributions' published minimums:
//   - k3s (docs.k3s.io/installation/requirements): server 2 CPUs / 2 GB, agent 1 CPU / 512 MB
//   - kubeadm (kubernetes.io install-kubeadm): 2 GB on every machine, 2 CPUs on control planes
//
// Neither publishes a disk minimum: DiskGB is tow's own estimate of free space for the
// distribution, its images and the kubelet's eviction margin.
type Minimum struct {
	What     string // "k3s server", for messages
	CPUs     int
	MemoryMB int // nominal, as sold: 2048 for "2 GB"
	DiskGB   int // free space under /var
	Archs    []string
}

// osDiskGB is room for the operating system itself, on top of DiskGB, when tow sizes a
// whole disk (provisioned machines).
const osDiskGB = 4

// MinimumFor returns the minimum machine for a distribution and role; false for local
// distributions, which don't run on machines of their own.
func MinimumFor(distribution, role string) (Minimum, bool) {
	cp := role == RoleControlPlane
	switch distribution {
	case DistributionK3s:
		m := Minimum{What: "k3s agent", CPUs: 1, MemoryMB: 512, DiskGB: 4, Archs: []string{"x86_64", "aarch64", "armv7l"}}
		if cp {
			m.What, m.CPUs, m.MemoryMB = "k3s server", 2, 2048
		}
		return m, true
	case DistributionKubeadm:
		m := Minimum{What: "kubeadm worker", CPUs: 1, MemoryMB: 2048, DiskGB: 10, Archs: []string{"x86_64", "aarch64"}}
		if cp {
			m.What, m.CPUs = "kubeadm control plane", 2
		}
		return m, true
	}
	return Minimum{}, false
}

// MinDiskGB is the smallest whole disk tow creates for the role: the free space the
// distribution needs plus the operating system.
func (m Minimum) MinDiskGB() int { return m.DiskGB + osDiskGB }
