package prepare

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// RecordFile lists, on each machine, every change tow made there and how to undo it,
// Kubernetes itself included. 'tow delete' undoes them from it.
const RecordFile = "/etc/tow/prepared.json"

// Record is one change made on a machine.
type Record struct {
	Cluster string `json:"cluster"`
	What    string `json:"what"`
	Detail  string `json:"detail"`
	Undo    string `json:"undo"` // how to undo it, for a person
	// Revert undoes it: a script 'tow delete' runs as root, which does nothing when there
	// is nothing left to undo. Records from before revert scripts have none; delete lists
	// them for the user to undo by hand.
	Revert string `json:"revert,omitempty"`
	// Kubernetes marks what installing Kubernetes changed (the distribution, its packages, the
	// container runtime): 'delete' undoes it. The rest is preparation: 'delete --reset'.
	Kubernetes bool      `json:"kubernetes,omitempty"`
	At         time.Time `json:"at"`
}

type ledger struct {
	Changes []Record `json:"changes"`
}

// Recorded reports whether a machine's record (the file's content) lists a change match
// accepts.
func Recorded(existing []byte, match func(Record) bool) bool {
	var l ledger
	if json.Unmarshal(existing, &l) != nil {
		return false
	}
	for _, r := range l.Changes {
		if match(r) {
			return true
		}
	}
	return false
}

// RecordScript appends records to what the machine already has and writes the file back.
// Nothing in it is secret, but its firewall rules name the user's address: only root reads
// it (tow reads it as root), and it's never readable by others, not even while written.
func RecordScript(existing []byte, add []Record) (string, error) {
	var l ledger
	keep := ""
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &l); err != nil {
			// Keep what's there under a name of its own rather than lose it: .prev is
			// overwritten by the next write.
			l = ledger{}
			keep = "cp -p " + RecordFile + " " + RecordFile + ".unreadable-$(date +%Y%m%d%H%M%S)\n"
		}
	}
	l.Changes = append(l.Changes, add...)
	return l.script(keep)
}

// script writes the ledger to the machine, or removes the file once it lists nothing.
func (l ledger) script(keep string) (string, error) {
	if len(l.Changes) == 0 {
		return "rm -f " + RecordFile + " " + RecordFile + ".prev\nrmdir /etc/tow 2>/dev/null || true\n", nil
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return "", err
	}
	if strings.Contains(string(data), "TOW_EOF") {
		return "", fmt.Errorf("record contains the heredoc marker")
	}
	// Root-only first, so the copies below (cp -p keeps the mode) are too: a file from an
	// older tow was readable by everyone.
	return "mkdir -p /etc/tow\n" +
		"[ -f " + RecordFile + " ] && chmod 0600 " + RecordFile + "\n" + keep +
		"[ -f " + RecordFile + " ] && cp -p " + RecordFile + " " + RecordFile + ".prev\n" +
		"(umask 077; cat > " + RecordFile + " <<'TOW_EOF'\n" + string(data) + "\nTOW_EOF\n)\n" +
		"chmod 0600 " + RecordFile + "\n", nil
}

// RemovePackages is the revert script for packages tow installed with apt: removed
// (purged, for its own Kubernetes packages) when still installed; one already gone is skipped.
func RemovePackages(verb string, pkgs []string) string {
	return "pkgs=$(dpkg-query -W -f='${Package} ${Status}\\n' " + strings.Join(pkgs, " ") + " 2>/dev/null | awk '$4 == \"installed\" {print $1}')\n" +
		"[ -z \"$pkgs\" ] || DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=600 " + verb + " -y -qq $pkgs\n"
}

// RemoveRPMs is the revert script for packages tow installed with dnf: removed when still
// installed; one already gone is skipped. A package built for a kernel is named with that
// kernel's release, as installed: after a reboot into another kernel, the build tow
// installed is still the one removed.
func RemoveRPMs(specs []string) string {
	var b strings.Builder
	b.WriteString("pkgs=\n")
	for _, s := range specs {
		fmt.Fprintf(&b, "p=%s; rpm -q --quiet \"$p\" && pkgs=\"$pkgs $p\"\n", s)
	}
	b.WriteString("[ -z \"$pkgs\" ] || dnf remove -y -q $pkgs\n")
	return b.String()
}
