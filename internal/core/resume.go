package core

import (
	"strings"

	"github.com/towproj/tow/internal/state"
)

// stagesDone names what each stage of an apply leaves behind, in order.
var stagesDone = []struct{ stage, what string }{
	{state.StageMachines, "the machines"},
	{state.StagePrepared, "their preparation"},
	{state.StageKubernetes, "Kubernetes"},
	{state.StageAddons, "the add-ons"},
}

// pickUp says what an apply that stopped after stage had done, which it keeps: running it
// again skips that and carries on. The command adds the exact command line to run
// (cmd/apply.go), so every failed run ends with it.
func pickUp(stage string) string {
	var done []string
	for _, s := range stagesDone {
		done = append(done, s.what)
		if s.stage == stage {
			return "\nDone and kept: " + strings.Join(done, ", ") + ". Run it again to pick up from here: what's done is skipped."
		}
	}
	return ""
}

// stoppedAt says what an update of a running cluster had done when it stopped. The plan is
// made again from what's really there, so those changes don't come back.
func stoppedAt(done []string) string {
	if len(done) == 0 {
		return "\nNo step finished before it stopped; the one that failed is planned again when you run it again."
	}
	return "\nDone before it stopped, so left out of the plan when you run it again:\n  - " + strings.Join(done, "\n  - ")
}
