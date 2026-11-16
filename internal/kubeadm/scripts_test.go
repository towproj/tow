package kubeadm

import (
	"os/exec"
	"testing"

	"github.com/towproj/tow/internal/prepare"
)

// What installing Kubernetes changed is marked as such, so 'delete' undoes it, with scripts
// that parse.
func TestRecordsRevert(t *testing.T) {
	rocky := facts{family: prepare.FamilyRHEL, major: 10}
	for _, r := range []prepare.Record{packagesRecord("v1.37.2", facts{}), containerdRecord("2.3.7"), crioRecord(37, facts{}),
		packagesRecord("v1.37.2", rocky), crioRecord(37, rocky)} {
		if !r.Kubernetes || r.Revert == "" {
			t.Errorf("%s: kubernetes %v, revert %q", r.What, r.Kubernetes, r.Revert)
		}
		if out, err := exec.Command("sh", "-n", "-c", "set -e\n"+r.Revert).CombinedOutput(); err != nil {
			t.Errorf("%s revert doesn't parse: %v\n%s\n%s", r.What, err, out, r.Revert)
		}
	}
}
