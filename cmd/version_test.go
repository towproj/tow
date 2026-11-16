package cmd

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestVersionCommand(t *testing.T) {
	var out bytes.Buffer
	rootCmd.SetArgs([]string{"version"})
	rootCmd.SetOut(&out)
	rootCmd.SetErr(io.Discard)
	t.Cleanup(func() { rootCmd.SetOut(nil) })
	if err := rootCmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.HasPrefix(s, "tow ") || !strings.Contains(s, "platform: ") {
		t.Errorf("version:\n%s", s)
	}
	if err := runCLI(t, "version", "extra"); err == nil {
		t.Error("version takes no arguments")
	}
}
