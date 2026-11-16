package prepare

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/towproj/tow/internal/parallel"
	"github.com/towproj/tow/internal/providers"
)

// UndoOptions say whose changes to undo, and on which machines.
type UndoOptions struct {
	Cluster    string
	Nodes      []providers.Node
	KnownHosts string
	Connect    func(ctx context.Context, n providers.Node, knownHosts string) (Runner, error)
	// Firewall undoes the cluster's firewall rules too when the rest of the preparation
	// stays: a machine leaving a cluster that keeps running.
	Firewall bool
}

// Left is what a cluster left on one machine, as the machine's record says: what installing
// Kubernetes changed, and what preparing the machine did, each newest first.
type Left struct {
	Node        providers.Node
	Kubernetes  []Record
	Preparation []Record
}

// Manual lists the records among these that have no revert script: the user undoes them.
func Manual(rs []Record) []Record {
	var out []Record
	for _, r := range rs {
		if r.Revert == "" {
			out = append(out, r)
		}
	}
	return out
}

// undoNode is one machine's progress.
type undoNode struct {
	n      providers.Node
	r      Runner
	ledger ledger
	left   Left
	err    error
}

func (x *undoNode) Label() string { return x.n.Label() }
func (x *undoNode) Err() error    { return x.err }

// read connects and reads the machine's record.
func (x *undoNode) read(ctx context.Context, o UndoOptions) {
	r, err := o.Connect(ctx, x.n, o.KnownHosts)
	if err != nil {
		x.err = err
		return
	}
	x.r = r
	out, err := x.r.RunInput(ctx, asRoot, "cat "+RecordFile+" 2>/dev/null || true\n")
	if err != nil {
		x.err = fmt.Errorf("read %s: %w", RecordFile, err)
		return
	}
	if strings.TrimSpace(out) == "" {
		return // nothing recorded: tow changed nothing here
	}
	if err := json.Unmarshal([]byte(out), &x.ledger); err != nil {
		x.err = fmt.Errorf("%s can't be read (%v): tow can't tell what it changed there", RecordFile, err)
		return
	}
	x.left = Left{Node: x.n}
	for _, r := range slices.Backward(x.ledger.Changes) {
		switch {
		case r.Cluster != o.Cluster:
		case r.Kubernetes:
			x.left.Kubernetes = append(x.left.Kubernetes, r)
		default:
			x.left.Preparation = append(x.left.Preparation, r)
		}
	}
}

func (o UndoOptions) nodes() []*undoNode {
	xs := make([]*undoNode, len(o.Nodes))
	for i, n := range o.Nodes {
		xs[i] = &undoNode{n: n}
	}
	return xs
}

func closeAll(xs []*undoNode) {
	for _, x := range xs {
		if x.r != nil {
			x.r.Close()
		}
	}
}

// ReadLeft reads, on every machine at once, what the cluster left there. A machine that
// can't be reached, or whose record can't be read, is an error: nothing is known about it.
func ReadLeft(ctx context.Context, o UndoOptions) ([]Left, error) {
	if o.Connect == nil {
		o.Connect = Connect
	}
	xs := o.nodes()
	defer closeAll(xs)
	parallel.Each(xs, func(x *undoNode) { x.read(ctx, o) })
	if err := parallel.Errors(xs, "could not read what tow changed on"); err != nil {
		return nil, err
	}
	out := make([]Left, len(xs))
	for i, x := range xs {
		out[i] = x.left
		out[i].Node = x.n
	}
	return out, nil
}

// Undo undoes, on every machine at once, what the cluster left there: Kubernetes, then the
// preparation too when preparation is set. Each change leaves the record once undone, so
// running Undo again after a failure picks up where it stopped. Changes recorded without a
// revert script stay in the record, for the user to undo (Manual lists them).
func Undo(ctx context.Context, o UndoOptions, preparation bool) error {
	if o.Connect == nil {
		o.Connect = Connect
	}
	xs := o.nodes()
	defer closeAll(xs)
	parallel.Each(xs, func(x *undoNode) {
		ctx, cancel := context.WithTimeout(ctx, nodeTimeout)
		defer cancel()
		if x.read(ctx, o); x.err != nil {
			return
		}
		todo := slices.Clone(x.left.Kubernetes)
		switch {
		case preparation:
			todo = append(todo, x.left.Preparation...)
		case o.Firewall:
			for _, r := range x.left.Preparation {
				if r.What == firewallRecord {
					todo = append(todo, r)
				}
			}
		}
		var undone []Record
		for _, r := range todo {
			if r.Revert == "" {
				continue
			}
			if out, err := x.r.RunInput(ctx, asRoot, "set -e\n"+r.Revert); err != nil {
				x.err = fmt.Errorf("undo %s: %w\n%s", r.What, err, strings.TrimSpace(out))
				break
			}
			undone = append(undone, r)
		}
		if len(undone) == 0 {
			return
		}
		// What was undone leaves the record, even after a failure: the next run skips it.
		x.ledger.Changes = slices.DeleteFunc(x.ledger.Changes, func(r Record) bool { return slices.Contains(undone, r) })
		script, err := x.ledger.script("")
		if err == nil {
			_, err = x.r.RunInput(ctx, asRoot, "set -e\n"+script)
		}
		if err != nil && x.err == nil {
			x.err = fmt.Errorf("changes undone, but %s could not be updated: %w", RecordFile, err)
		}
	})
	return parallel.Errors(xs, "could not undo what tow changed on")
}
