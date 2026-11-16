package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/towproj/tow/internal/addons"
	"github.com/towproj/tow/internal/providers"
)

const (
	rootBase = "~/.tow"
	lockDir  = "locks" // <name>.lock: one run changes a cluster at a time (Lock)

	// CurrentVersion is the state file format. 0 means a Phase 0 file written before versioning.
	CurrentVersion = 1

	StatusReady = "ready"
	// StatusFailed marks a provision that stopped partway: some machines may exist, so
	// 'apply' retries and 'delete' cleans up instead of treating the cluster as unknown.
	StatusFailed = "failed"

	// Stages an apply goes through; a failed apply resumes after the last one it finished.
	StageMachines   = "machines"   // the machines exist (or were checked, byoh)
	StagePrepared   = "prepared"   // they're ready for Kubernetes (packages, kernel, swap, firewall)
	StageKubernetes = "kubernetes" // Kubernetes runs on them and the kubeconfig is handed out
	StageAddons     = "addons"     // the file's Helm charts are installed
)

// Root is tow's home directory; nothing in it belongs in a git repository.
func Root() string { return expandHome(rootBase) }

// Dir holds everything tow keeps for one cluster: state, OpenTofu files, pinned host keys.
func Dir(name string) string { return filepath.Join(Root(), "clusters", name) }

// BackupsDir is where a cluster's backups are kept on this machine: outside its state folder,
// so they outlive the cluster.
func BackupsDir(name string) string { return filepath.Join(Root(), "backups", name) }

type ClusterState struct {
	StateVersion int       `json:"state_version"`
	Name         string    `json:"name"`
	Provider     string    `json:"provider"`
	Mode         string    `json:"mode,omitempty"`
	K8sVersion   string    `json:"k8s_version,omitempty"` // resolved release actually installed
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Status       string    `json:"status"`
	Stage        string    `json:"stage,omitempty"` // the last stage finished

	// What was applied, for drift detection.
	ConfigHash string            `json:"config_hash,omitempty"`
	Config     map[string]string `json:"config,omitempty"` // effective config fields, to show what changed
	// File is the effective config as YAML (config.Restore reads it back): what the next
	// apply compares the built-in add-ons' settings with.
	File string `json:"file,omitempty"`

	// What the provider was asked for, so status and delete work without the config file.
	Spec *providers.Spec `json:"spec,omitempty"`

	// What the provider produced: nodes, endpoint, kube context.
	Outputs *providers.Result `json:"outputs,omitempty"`
	// Manifests are the objects each of the file's manifests made, by its path: what apply
	// deletes when a manifest drops them or leaves the file.
	Manifests map[string][]addons.Object `json:"manifests,omitempty"`
	// Cordoned are the nodes an upgrade drained and hasn't uncordoned yet (it stopped): the
	// next run finishes them, even when they already run the new release.
	Cordoned []string `json:"cordoned,omitempty"`
	// CiliumCompat is the minor release Cilium was first installed at ("1.19"), known from
	// its first upgrade: its later installs keep that release's defaults (upgradeCompatibility).
	CiliumCompat string `json:"cilium_compat,omitempty"`
}

// Save writes atomically (temp file + rename) so a crash can't leave a half-written state file.
func Save(name string, st ClusterState) error {
	dir := Dir(name)
	// Owner-only: state will soon carry kubeconfigs and join tokens.
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	st.StateVersion = CurrentVersion
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, "state-*.json.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed

	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, "state.json"))
}

// Load returns an error wrapping fs.ErrNotExist when the cluster has no state.
func Load(name string) (*ClusterState, error) {
	path := filepath.Join(Dir(name), "state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var st ClusterState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func Exists(name string) bool {
	_, err := Load(name)
	return err == nil
}

// List returns the names of every cluster with saved state, sorted. Directories without a
// state file (only pinned host keys from a failed byoh check) are not clusters.
func List() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(Root(), "clusters"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries { // ReadDir sorts by name
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(Dir(e.Name()), "state.json")); err == nil {
				names = append(names, e.Name())
			}
		}
	}
	return names, nil
}

// Remove deletes the cluster's whole directory. The name check keeps a bad --name
// from ever resolving to the clusters directory or anything above it.
func Remove(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return fmt.Errorf("invalid cluster name %q", name)
	}
	return os.RemoveAll(Dir(name))
}

// expandHome handles ~ prefix since os/user doesn't cover it in stdlib.
func expandHome(p string) string {
	if home, err := os.UserHomeDir(); err == nil && len(p) > 0 && p[0] == '~' {
		return filepath.Join(home, p[1:])
	}
	return p
}
