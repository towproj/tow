// Package parallel runs work on several machines at once and gathers what went wrong, for
// every package that works on machines (prepare, k3s, kubeadm, byoh).
package parallel

import (
	"errors"
	"fmt"
	"sync"
)

// Each runs fn for every item at once and waits for all of them. fn may only change its own
// item.
func Each[T any](items []T, fn func(T)) {
	var wg sync.WaitGroup
	for _, x := range items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn(x)
		}()
	}
	wg.Wait()
}

// Item is one machine's work: what to call it, and how it went.
type Item interface {
	Label() string
	Err() error
}

// Errors joins every item's error, each as "<what> <label>: <error>", in the items' order;
// nil when all of them went well.
func Errors[T Item](items []T, what string) error {
	var errs []error
	for _, x := range items {
		if err := x.Err(); err != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w", what, x.Label(), err))
		}
	}
	return errors.Join(errs...)
}
