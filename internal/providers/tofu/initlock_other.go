//go:build !unix

package tofu

import "context"

// lockInit is a no-op where flock doesn't exist; the modules' lock files still keep one init
// from rewriting a provider another is running.
func lockInit(context.Context, string) (func(), error) { return func() {}, nil }
