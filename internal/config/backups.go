package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Sources (read 2026-11-13), followed here:
//   - k3s: etcd-snapshot-schedule-cron (default "0 */12 * * *") and etcd-snapshot-retention
//     (default 5)  https://docs.k3s.io/cli/etcd-snapshot
//   - systemd.time(7): OnCalendar takes "weekdays year-month-day hour:minute:second"; values
//     are lists ("a,b"), ranges ("a..b") and repetitions ("start/step"); with both a weekday
//     and a day of the month, both must match
//   - cron(5): with both a day of the month and a weekday restricted, either may match; 0 and
//     7 are Sunday

// Backups are etcd snapshots taken on the control planes on a schedule.
type Backups struct {
	Schedule string `yaml:"schedule,omitempty"` // 5 fields, as cron: "0 */6 * * *", in the machines' time zone (UTC on cloud images)
	Keep     int    `yaml:"keep,omitempty"`     // the newest snapshots kept on each control plane; default 5
}

// DefaultBackupsKeep is how many snapshots a control plane keeps when the file says nothing
// (k3s's own default).
const DefaultBackupsKeep = 5

var cronField = regexp.MustCompile(`^(\*|\d+(-\d+)?)(/\d+)?(,(\*|\d+(-\d+)?)(/\d+)?)*$`)

// cronFields splits a schedule into its five fields, checked one by one.
func cronFields(schedule string) ([]string, error) {
	f := strings.Fields(schedule)
	if len(f) != 5 {
		return nil, fmt.Errorf("%q has %d fields; it takes 5, as cron: minute hour day-of-month month weekday, like \"0 */6 * * *\"", schedule, len(f))
	}
	limits := []struct {
		name     string
		min, max int
	}{{"minute", 0, 59}, {"hour", 0, 23}, {"day of the month", 1, 31}, {"month", 1, 12}, {"weekday", 0, 7}}
	for i, field := range f {
		if !cronField.MatchString(field) {
			return nil, fmt.Errorf("%q: the %s %q takes numbers, *, ranges (a-b), steps (/n) and lists (a,b)", schedule, limits[i].name, field)
		}
		for _, part := range strings.Split(field, ",") {
			base, step, stepped := strings.Cut(part, "/")
			if s, _ := strconv.Atoi(step); stepped && s < 1 {
				return nil, fmt.Errorf("%q: the %s's step /%s must be 1 or more", schedule, limits[i].name, step)
			}
			var ends []int
			for _, n := range strings.Split(base, "-") {
				if n == "*" {
					continue
				}
				v, _ := strconv.Atoi(n)
				if v < limits[i].min || v > limits[i].max {
					return nil, fmt.Errorf("%q: the %s goes from %d to %d", schedule, limits[i].name, limits[i].min, limits[i].max)
				}
				ends = append(ends, v)
			}
			if len(ends) == 2 && ends[0] > ends[1] {
				return nil, fmt.Errorf("%q: the %s range %s goes backwards", schedule, limits[i].name, base)
			}
		}
	}
	return f, nil
}

// OnCalendar turns a 5-field cron schedule into systemd's OnCalendar: "0 */6 * * *" is
// "*-*-* 0/6:0:00". A schedule systemd would read differently is refused: a day of the month
// and a weekday both restricted (cron runs when either matches, systemd when both do), a step
// on a range.
func OnCalendar(schedule string) (string, error) {
	f, err := cronFields(schedule)
	if err != nil {
		return "", err
	}
	if f[2] != "*" && f[4] != "*" {
		return "", fmt.Errorf("%q restricts both the day of the month and the weekday: cron runs when either matches, a systemd timer only when both do; use one of them", schedule)
	}
	value := func(field string, first int) (string, error) {
		var parts []string
		for _, p := range strings.Split(field, ",") {
			base, step, stepped := strings.Cut(p, "/")
			switch {
			case stepped && strings.Contains(base, "-"):
				return "", fmt.Errorf("%q: a step on a range (%s) has no systemd form; list the values instead", schedule, p)
			case stepped && base == "*":
				parts = append(parts, fmt.Sprintf("%d/%s", first, step))
			case stepped:
				parts = append(parts, base+"/"+step)
			default:
				parts = append(parts, strings.Replace(base, "-", "..", 1))
			}
		}
		return strings.Join(parts, ","), nil
	}
	var out [5]string
	for i, first := range []int{0, 0, 1, 1} {
		if out[i], err = value(f[i], first); err != nil {
			return "", err
		}
	}
	calendar := fmt.Sprintf("*-%s-%s %s:%s:00", out[3], out[2], out[1], out[0])
	if f[4] == "*" {
		return calendar, nil
	}
	days := []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	var weekdays []string
	for _, p := range strings.Split(f[4], ",") {
		if strings.Contains(p, "/") {
			return "", fmt.Errorf("%q: a step on weekdays has no systemd form; list the days instead", schedule)
		}
		if p == "*" {
			return calendar, nil
		}
		a, b, isRange := strings.Cut(p, "-")
		x, _ := strconv.Atoi(a)
		y := x
		if isRange {
			y, _ = strconv.Atoi(b)
		}
		// systemd's week starts on Monday: a range from Sunday (0) is Sunday plus the rest.
		if x == 0 {
			weekdays = append(weekdays, "Sun")
			if x++; y < x {
				continue
			}
		}
		if x == y {
			weekdays = append(weekdays, days[x])
			continue
		}
		weekdays = append(weekdays, days[x]+".."+days[y])
	}
	return strings.Join(weekdays, ",") + " " + calendar, nil
}

func (c *Config) validateBackups() []error {
	b := c.Spec.Kubernetes.Backups
	if b == nil {
		return nil
	}
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	switch {
	case c.Mode == ModeLocal:
		add("spec.kubernetes.backups: a local cluster has no etcd snapshots for tow to schedule")
		return errs
	case b.Schedule == "":
		add("spec.kubernetes.backups.schedule is required: when to take a snapshot, as cron, like \"0 */6 * * *\"")
	case c.Spec.Kubernetes.Distribution == DistributionKubeadm:
		if _, err := OnCalendar(b.Schedule); err != nil {
			add("spec.kubernetes.backups.schedule %v", err)
		}
	default:
		if _, err := cronFields(b.Schedule); err != nil {
			add("spec.kubernetes.backups.schedule %v", err)
		}
	}
	if b.Keep < 1 || b.Keep > 100 {
		add("spec.kubernetes.backups.keep %d must be from 1 to 100: the newest snapshots each control plane keeps", b.Keep)
	}
	return errs
}
