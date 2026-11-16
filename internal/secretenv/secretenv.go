// Package secretenv keeps the environment variables that hold the user's secrets away from
// the programs tow starts (tofu, k3d, docker): only the step that needs a secret
// reads it, and the value goes nowhere else.
package secretenv

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/towproj/tow/internal/ui"
)

var (
	mu     sync.RWMutex
	hidden = map[string]bool{}
)

// Hide marks variables as holding secrets, for every program started from now on.
func Hide(names ...string) {
	mu.Lock()
	defer mu.Unlock()
	for _, n := range names {
		hidden[n] = true
	}
}

// Read returns a secret variable's value and hides it from all of tow's output from now
// on. Unset or empty is an error naming the variable, never showing anything else.
func Read(name string) (string, error) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return "", fmt.Errorf("environment variable %s isn't set: the cluster file reads a secret from it; set it, then run tow again", name)
	}
	ui.RegisterSecret(v)
	Hide(name)
	return v, nil
}

// Environ is this process's environment without the hidden variables: give it to every
// program tow starts (exec.Cmd.Env).
func Environ() []string {
	mu.RLock()
	defer mu.RUnlock()
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !hidden[name] {
			out = append(out, kv)
		}
	}
	return out
}
