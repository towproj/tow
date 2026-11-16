package parallel

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type job struct {
	name string
	err  error
	ran  bool
}

func (j *job) Label() string { return j.name }
func (j *job) Err() error    { return j.err }

// Every item runs, at the same time, and Each returns only once all of them finished.
func TestEach(t *testing.T) {
	jobs := []*job{{name: "a"}, {name: "b"}, {name: "c"}}
	var running, most atomic.Int32
	Each(jobs, func(j *job) {
		n := running.Add(1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		running.Add(-1)
		j.ran = true
	})
	for _, j := range jobs {
		if !j.ran {
			t.Errorf("%s didn't run", j.name)
		}
	}
	if most.Load() < 2 {
		t.Errorf("at most %d ran at once", most.Load())
	}
}

// The failures, in order, each named; nil when none failed.
func TestErrors(t *testing.T) {
	boom := errors.New("boom")
	jobs := []*job{{name: "a"}, {name: "b", err: boom}, {name: "c", err: errors.New("bang")}}
	err := Errors(jobs, "could not check")
	if err == nil || err.Error() != "could not check b: boom\ncould not check c: bang" || !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
	if err := Errors([]*job{{name: "a"}}, "x"); err != nil {
		t.Errorf("no failure: %v", err)
	}
}
