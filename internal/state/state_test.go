package state

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/towproj/tow/internal/providers"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	in := ClusterState{
		Name:       "lab",
		Provider:   "k3d",
		ConfigHash: "abc",
		Config:     map[string]string{"cni": "flannel"},
		Outputs:    &providers.Result{KubeContext: "k3d-lab"},
	}
	if err := Save("lab", in); err != nil {
		t.Fatal(err)
	}

	out, err := Load("lab")
	if err != nil {
		t.Fatal(err)
	}
	if out.StateVersion != CurrentVersion || out.ConfigHash != "abc" || out.Config["cni"] != "flannel" || out.Outputs.KubeContext != "k3d-lab" {
		t.Errorf("round trip lost data: %+v", out)
	}

	dir := filepath.Join(home, ".tow/clusters/lab")
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Errorf("expected only state.json (no temp files left), got %v", entries)
	}
	info, _ := os.Stat(filepath.Join(dir, "state.json"))
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("state.json mode = %o, want 600", perm)
	}
}

// A file without a state_version reads as version 0.
func TestLoadUnversionedFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".tow/clusters/dev")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"name":"dev","provider":"k3d","status":"ready","created_at":"2026-09-01T10:00:00Z","updated_at":"2026-09-01T10:00:00Z"}`), 0600)

	st, err := Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	if st.StateVersion != 0 || st.ConfigHash != "" || st.Provider != "k3d" {
		t.Errorf("phase 0 file parsed wrong: %+v", st)
	}
}

func TestLoadMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := Load("nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestList(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if names, err := List(); err != nil || len(names) != 0 {
		t.Fatalf("no clusters yet: %v, %v", names, err)
	}

	Save("b", ClusterState{Name: "b"})
	Save("a", ClusterState{Name: "a"})
	// A failed byoh check leaves only pinned host keys: not a cluster.
	os.MkdirAll(Dir("keys-only"), 0700)
	os.WriteFile(filepath.Join(Dir("keys-only"), "known_hosts"), nil, 0600)

	names, err := List()
	if err != nil || len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Errorf("List() = %v, %v; want [a b]", names, err)
	}
}
