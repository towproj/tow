package ui

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// Everything tow prints goes through Redact, so a secret in a tool's output or an error
// message never reaches the terminal or a CI log.
func Info(msg string)   { fmt.Printf("✓ %s\n", Redact(msg)) }
func Hint(msg string)   { fmt.Printf("  → %s\n", Redact(msg)) }
func DryRun(msg string) { fmt.Printf("[DRY RUN] %s\n", Redact(msg)) }

// Warn reports something that doesn't stop tow but weakens what it promises.
func Warn(msg string) { fmt.Fprintf(os.Stderr, "! %s\n", Redact(msg)) }

// Error logs to stderr and returns error for caller to control exit flow.
func Error(msg string, hint string) error {
	fmt.Fprintf(os.Stderr, "✗ %s\n", Redact(msg))
	if hint != "" {
		fmt.Fprintf(os.Stderr, "  → Hint: %s\n", Redact(hint))
	}
	return errors.New(msg)
}

// Step returns a closure to mark completion. Keeps output aligned during blocking ops.
func Step(label string) func() {
	fmt.Printf("⏳ %s... ", Redact(label))
	return func() { fmt.Println("done") }
}

// StepRun shows label, runs fn, then says done, or failed and returns fn's error.
func StepRun(label string, fn func() error) error {
	done := Step(label)
	if err := fn(); err != nil {
		fmt.Println("failed")
		return err
	}
	done()
	return nil
}

// Plural counts a word: "1 machine", "3 machines".
func Plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// LastLines is the last n lines of a program's output.
func LastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Indent sets every line of s two spaces in, under the message that quotes it.
func Indent(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  ")
}

var verbose, nonInteractive bool

func SetVerbose(v bool) { verbose = v }
func Verbose() bool     { return verbose }

func SetNonInteractive(v bool) { nonInteractive = v }

// Interactive reports whether someone can answer a prompt: stdin is a terminal and
// --non-interactive is off. CI, cron and service accounts get errors, never a hang.
func Interactive() bool { return !nonInteractive && term.IsTerminal(int(os.Stdin.Fd())) }

var stdin = bufio.NewReader(os.Stdin)

// Retry shows a problem the user can fix in another terminal (e.g. logging in) and waits
// for Enter. It returns false at once when nobody can answer, so the caller fails instead.
func Retry(problem string) bool {
	if !Interactive() {
		return false
	}
	fmt.Fprintf(os.Stderr, "✗ %s\n  → Fix it in another terminal, then press Enter to retry (Ctrl+C to abort) ", Redact(problem))
	_, err := stdin.ReadString('\n')
	return err == nil
}

// Confirm asks a yes/no question; anything but y or yes is a no.
func Confirm(question string) bool {
	fmt.Printf("? %s [y/N] ", question)
	answer, _ := stdin.ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

func Debug(msg string, args ...interface{}) {
	if verbose {
		fmt.Println(Redact(fmt.Sprintf("[DEBUG] "+msg, args...)))
	}
}
