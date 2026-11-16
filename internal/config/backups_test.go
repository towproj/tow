package config

import (
	"os/exec"
	"strings"
	"testing"
)

// cron schedules as systemd timers run them; what systemd would read differently is refused.
func TestOnCalendar(t *testing.T) {
	for cron, want := range map[string]string{
		"0 */6 * * *":       "*-*-* 0/6:0:00",
		"30 2 * * *":        "*-*-* 2:30:00",
		"*/15 * * * *":      "*-*-* *:0/15:00",
		"0 1,13 * * *":      "*-*-* 1,13:0:00",
		"0 3 1 * *":         "*-*-1 3:0:00",
		"0 3 */2 1-6 *":     "*-1..6-1/2 3:0:00",
		"30 2 * * 1-5":      "Mon..Fri *-*-* 2:30:00",
		"0 0 * * 0,6":       "Sun,Sat *-*-* 0:0:00",
		"0 0 * * 7":         "Sun *-*-* 0:0:00",
		"*/2 * * * *":       "*-*-* *:0/2:00",
		"5 4 * * *":         "*-*-* 4:5:00",
		"0 0 1,15 * *":      "*-*-1,15 0:0:00",
		"0 12 * 1,4,7,10 *": "*-1,4,7,10-* 12:0:00",
		// systemd's week starts on Monday: Sunday leaves a range that starts with it.
		"0 0 * * 0-2": "Sun,Mon..Tue *-*-* 0:0:00",
		"0 0 * * 0-1": "Sun,Mon *-*-* 0:0:00",
		"0 0 * * 0-0": "Sun *-*-* 0:0:00",
		"0 0 * * 5-7": "Fri..Sun *-*-* 0:0:00",
	} {
		got, err := OnCalendar(cron)
		if err != nil || got != want {
			t.Errorf("OnCalendar(%q) = %q, %v; want %q", cron, got, err, want)
			continue
		}
		// systemd itself reads it, where it's installed.
		if out, err := exec.Command("systemd-analyze", "calendar", got).CombinedOutput(); err == nil || !strings.Contains(err.Error(), "executable file not found") {
			if err != nil {
				t.Errorf("systemd-analyze calendar %q: %v\n%s", got, err, out)
			}
		}
	}
	for cron, why := range map[string]string{
		"0 0 1 * 1":      "either matches",
		"0 0-12/2 * * *": "a step on a range",
		"0 0 * * */2":    "a step on weekdays",
		"0 0 * *":        "it takes 5",
		"61 * * * *":     "the minute goes from 0 to 59",
		"0 @daily * * *": "takes numbers",
		"0 5-1 * * *":    "the hour range 5-1 goes backwards",
		"*/0 * * * *":    "the minute's step /0 must be 1 or more",
		"0 0 32 * *":     "the day of the month goes from 1 to 31",
	} {
		if _, err := OnCalendar(cron); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("OnCalendar(%q): %v, want %q", cron, err, why)
		}
	}
}

func TestBackups(t *testing.T) {
	c := mustParse(t, header+"spec: {kubernetes: {distribution: k3s, backups: {schedule: \"0 */6 * * *\"}}, "+onAWS+"}\n")
	if c.Spec.Kubernetes.Backups.Keep != DefaultBackupsKeep {
		t.Errorf("keep defaults to %d, got %d", DefaultBackupsKeep, c.Spec.Kubernetes.Backups.Keep)
	}
	// k3s takes any cron schedule; kubeadm's systemd timer only those it reads the same way.
	mustParse(t, header+"spec: {kubernetes: {distribution: k3s, backups: {schedule: \"0 0 1 * 1\"}}, "+onAWS+"}\n")
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm, backups: {schedule: \"0 0 1 * 1\", keep: 0}}, "+onAWS+"}",
		"spec.kubernetes.backups.schedule \"0 0 1 * 1\" restricts both the day of the month and the weekday")
	expectErrors(t, "spec: {kubernetes: {distribution: kubeadm, backups: {keep: 500}}, "+onAWS+"}",
		"spec.kubernetes.backups.schedule is required",
		"spec.kubernetes.backups.keep 500 must be from 1 to 100")
	expectErrors(t, "spec: {kubernetes: {backups: {schedule: \"0 * * * *\"}}, infrastructure: {provider: local}}",
		"spec.kubernetes.backups: a local cluster has no etcd snapshots")
	expectErrors(t, "spec: {kubernetes: {distribution: k3s, k3s: {server: {extraArgs: {etcd-snapshot-retention: 3}}}}, "+onAWS+"}",
		"etcd-snapshot-retention conflicts with spec.kubernetes.backups.keep")
}
