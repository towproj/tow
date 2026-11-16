package cmd

import (
	"strings"
	"testing"
)

// Tests run without a terminal, which is exactly the CI case: no prompt, so --yes is required.
func TestDeleteNeedsYesWithoutTerminal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	err := runCLI(t, "delete", "--name", "dev")
	if err == nil || !strings.Contains(err.Error(), "pass --yes") {
		t.Errorf("err = %v, want a request for --yes", err)
	}

	err = runCLI(t, "delete", "--name", "dev", "--yes")
	if err == nil || !strings.Contains(err.Error(), "no cluster named 'dev'") {
		t.Errorf("err = %v, want unknown-cluster error", err)
	}

	// -v is delete's (--volumes), not the root's --version.
	err = runCLI(t, "delete", "--name", "dev", "--yes", "-v")
	if err == nil || !strings.Contains(err.Error(), "no cluster named 'dev'") {
		t.Errorf("err = %v, want unknown-cluster error", err)
	}

	// Undo the machines' preparation, or don't touch them at all: not both.
	err = runCLI(t, "delete", "--name", "dev", "--yes", "--reset", "--forget")
	if err == nil || !strings.Contains(err.Error(), "none of the others can be") {
		t.Errorf("err = %v, want reset and forget refused together", err)
	}
}

// status without --name lists the clusters, from state alone: none yet is no error.
func TestStatusListsClusters(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := runCLI(t, "status"); err != nil {
		t.Errorf("err = %v", err)
	}
	if err := runCLI(t, "status", "--name", "dev"); err == nil || !strings.Contains(err.Error(), "no cluster named 'dev'") {
		t.Errorf("err = %v", err)
	}
}
