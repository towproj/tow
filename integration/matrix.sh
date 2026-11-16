#!/bin/sh
# Runs every line of a test matrix (integration/proxmox-matrix.txt), one at a time, and sums
# them up. A failed line doesn't stop the others. From the repository's root:
#
#   integration/matrix.sh integration/proxmox-matrix.txt [text]
#
# With text, only the lines containing it run (to run again what failed). Each line's output
# goes to integration/logs/<date>/, with summary.txt; the exit code is 1 if any line failed.
# Nothing is read from the terminal, so it runs unattended (nohup, overnight).
set -u
list=$1
only=${2:-}
logs=integration/logs/$(date +%Y%m%d-%H%M%S)
mkdir -p "$logs"
summary=$logs/summary.txt

# One test binary for every line: built once, and runs from integration/, where the
# examples are ../examples.
(cd integration && go test -c -o integration.test .) || exit 1

n=0
failed=0
while IFS= read -r line; do
	case "$line" in '' | '#'*) continue ;; esac
	case "$line" in *"$only"*) ;; *) continue ;; esac
	n=$((n + 1))
	file=${line%% *}
	extra=
	[ "$file" = "$line" ] || extra=${line#* }
	log=$logs/$(printf '%02d' "$n")-$(basename "$file" .yaml).log
	start=$(date +%s)
	printf '%s  %s %s ... ' "$(date +%H:%M)" "$file" "$extra"
	# "lifecycle:k3s" (or kubeadm) runs a cluster's whole life instead (about an hour).
	test='^TestProxmoxInfra$' timeout=50m dist=
	case "$file" in lifecycle:*) test='^TestProxmoxLifecycle$' timeout=170m dist=${file#lifecycle:} ;; esac
	if (cd integration && INTEGRATION_PROXMOX=1 TOW_TEST_PROXMOX_CONFIG="../examples/$file" TOW_TEST_KUBERNETES="$extra" \
		TOW_TEST_LIFECYCLE="$dist" ./integration.test -test.v -test.count=1 -test.timeout "$timeout" -test.run "$test") >"$log" 2>&1 </dev/null; then
		result=PASS
	else
		result=FAIL
		failed=$((failed + 1))
	fi
	minutes=$((($(date +%s) - start + 59) / 60))
	echo "$result ($minutes min)"
	printf '%s  %3d min  %s %s  (%s)\n' "$result" "$minutes" "$file" "$extra" "$(basename "$log")" >>"$summary"
done <"$list"

echo
echo "$n run(s), $failed failed; logs in $logs"
cat "$summary" 2>/dev/null
[ "$failed" -eq 0 ]
