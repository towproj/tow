package secretenv

import (
	"slices"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/ui"
)

func TestHiddenVariablesNeverReachChildren(t *testing.T) {
	t.Setenv("TOW_TEST_SECRET", "fake-value")
	t.Setenv("TOW_TEST_PLAIN", "ok")
	Hide("TOW_TEST_SECRET")

	env := Environ()
	if slices.Contains(env, "TOW_TEST_SECRET=fake-value") {
		t.Error("a hidden variable reached the child environment")
	}
	if !slices.Contains(env, "TOW_TEST_PLAIN=ok") {
		t.Error("other variables must be kept")
	}
}

// Reading a secret hides it from every message tow prints afterwards.
func TestReadHidesTheValue(t *testing.T) {
	t.Setenv("TOW_TEST_READ", "fake-secret-0042")
	v, err := Read("TOW_TEST_READ")
	if err != nil || v != "fake-secret-0042" {
		t.Fatalf("Read = %q, %v", v, err)
	}
	if out := ui.Redact("k3d cluster create --k3s-arg --etcd-s3-secret-key=fake-secret-0042@server:*"); strings.Contains(out, "fake-secret-0042") {
		t.Errorf("the value still shows: %s", out)
	}
	if slices.Contains(Environ(), "TOW_TEST_READ=fake-secret-0042") {
		t.Error("a variable that was read must not reach child programs")
	}

	t.Setenv("TOW_TEST_EMPTY", "")
	if _, err := Read("TOW_TEST_EMPTY"); err == nil || !strings.Contains(err.Error(), "TOW_TEST_EMPTY isn't set") {
		t.Errorf("an empty variable must be refused by name: %v", err)
	}
	if _, err := Read("TOW_TEST_MISSING"); err == nil {
		t.Error("a missing variable must be an error")
	}
}
