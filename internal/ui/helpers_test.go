package ui

import (
	"errors"
	"testing"
)

func TestPlural(t *testing.T) {
	if got := Plural(1, "machine"); got != "1 machine" {
		t.Errorf("1: %q", got)
	}
	if got := Plural(3, "worker"); got != "3 workers" {
		t.Errorf("3: %q", got)
	}
}

func TestLastLinesAndIndent(t *testing.T) {
	out := "a\nb\nc\nd\n"
	if got := LastLines(out, 2); got != "c\nd" {
		t.Errorf("last 2: %q", got)
	}
	if got := LastLines(out, 10); got != "a\nb\nc\nd" {
		t.Errorf("last 10: %q", got)
	}
	if got := Indent("x\ny\n"); got != "  x\n  y" {
		t.Errorf("indent: %q", got)
	}
}

func TestStepRun(t *testing.T) {
	boom := errors.New("boom")
	if err := StepRun("failing", func() error { return boom }); err != boom {
		t.Errorf("err = %v", err)
	}
	if err := StepRun("working", func() error { return nil }); err != nil {
		t.Errorf("err = %v", err)
	}
}
