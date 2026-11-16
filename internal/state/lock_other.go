//go:build !unix

package state

// Lock is a no-op where flock doesn't exist: two runs on one cluster aren't stopped there.
func Lock(string) (func(), error) { return func() {}, nil }
