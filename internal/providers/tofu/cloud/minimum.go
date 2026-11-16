package cloud

import "github.com/towproj/tow/internal/config"

// Minimum is what a machine of a role needs on a public cloud, for the distribution and the
// machines' system. ok is false when the distribution states none.
func Minimum(engine, role, os string) (config.Minimum, bool) {
	min, ok := config.MinimumFor(engine, role)
	// AlmaLinux's cloud images reserve memory for kdump's crash kernel at boot: a 2 GiB machine
	// shows kubeadm about 1650 MB, under the 1700 MB its control plane checks for. The next
	// size up has room.
	if ok && config.RHELFamily(os) && engine == config.DistributionKubeadm && role == config.RoleControlPlane && min.MemoryMB < 3072 {
		min.MemoryMB = 3072
		min.What += " on " + os + " (its kernel keeps memory aside for kdump, so 2 GiB leaves less than kubeadm's 1700 MB)"
	}
	return min, ok
}
