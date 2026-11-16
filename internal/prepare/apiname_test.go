package prepare

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The scripts run for real, on a copy of a hosts file: tow's line is added, replaced, then
// removed, and every other line stays, a look-alike for another name included.
func TestHostsScripts(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("scripts target Linux nodes")
	}
	hosts := filepath.Join(t.TempDir(), "hosts")
	const others = "127.0.0.1 localhost\n10.0.0.9 api.lab.internal.example # someone else's\n10.0.0.8 apixlabxinternal\n"
	if err := os.WriteFile(hosts, []byte(others), 0644); err != nil {
		t.Fatal(err)
	}
	run := func(script string) string {
		t.Helper()
		if out, err := exec.Command("sh", "-c", strings.ReplaceAll(script, "/etc/hosts", hosts)).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		data, err := os.ReadFile(hosts)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	if got, want := run(HostsScript("api.lab.internal", "10.250.0.2")), others+"10.250.0.2 api.lab.internal # tow API name\n"; got != want {
		t.Errorf("added:\n%s\nwant:\n%s", got, want)
	}
	if got, want := run(HostsScript("api.lab.internal", "10.250.0.3")), others+"10.250.0.3 api.lab.internal # tow API name\n"; got != want {
		t.Errorf("replaced:\n%s\nwant:\n%s", got, want)
	}
	if got := run(HostsRevert("api.lab.internal")); got != others {
		t.Errorf("reverted:\n%s", got)
	}
	if got := run(HostsRevert("api.lab.internal")); got != others {
		t.Errorf("reverted twice:\n%s", got)
	}
}
