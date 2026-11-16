package ui

import (
	"bytes"
	"io"
	"regexp"
	"strings"
	"sync"
)

const redacted = "[REDACTED]"

// Secrets tow may see in tool output or errors, matched by shape. Phase 2 adds join
// tokens and kubeconfigs, so their shapes are covered before they ever appear.
var patterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// Whole PEM keys first, before any other pattern rewrites part of one.
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`), redacted},
	{regexp.MustCompile(`(?i)(authorization:\s*bearer\s+)\S+`), "${1}" + redacted},
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), redacted},   // AWS access key IDs
	{regexp.MustCompile(`\bK10[0-9a-f]{20,}::[^\s:]+:\S+`), redacted}, // k3s join tokens
	{regexp.MustCompile(`\b[a-z0-9]{6}\.[a-z0-9]{16}\b`), redacted},   // kubeadm bootstrap tokens
	// A kubeconfig's admin key (base64), in YAML or JSON.
	{regexp.MustCompile(`("?client-key-data"?\s*:\s*"?)[A-Za-z0-9+/=]+`), "${1}" + redacted},
}

// keyValue finds values after a credential-like key: key=value, key: value, "key": "value".
var keyValue = regexp.MustCompile(`(?i)("?[a-z0-9_.-]*(?:secret|password|passwd|token|credential|api[_-]?key|access[_-]?key|private[_-]?key)[a-z0-9_-]*"?\s*[:=]\s*"?)([^\s",}]*)`)

// notSecrets are values a credential-like key can hold that are never secrets, such as
// OpenTofu's 'http_tokens = "required"' (IMDSv2) or 'password_data = (known after apply)'.
// Hiding them would only make verbose output harder to read.
var notSecrets = map[string]bool{
	"": true, "true": true, "false": true, "null": true, "none": true,
	"required": true, "optional": true, "enabled": true, "disabled": true,
	"(known": true, "(sensitive": true, // "(known after apply)", "(sensitive value)"
}

var (
	secretsMu sync.RWMutex
	secrets   []string
)

// RegisterSecret hides an exact value from all output from now on, for secrets tow
// generates or receives at run time (join tokens, passwords). Very short values are ignored:
// they would blank out ordinary words.
func RegisterSecret(s string) {
	if len(s) < 8 {
		return
	}
	secretsMu.Lock()
	defer secretsMu.Unlock()
	secrets = append(secrets, s)
}

// Redact replaces every secret it recognizes in s with [REDACTED].
func Redact(s string) string {
	secretsMu.RLock()
	for _, v := range secrets {
		s = strings.ReplaceAll(s, v, redacted)
	}
	secretsMu.RUnlock()
	// PEM keys go first (patterns[0]), before keyValue could rewrite part of one.
	s = patterns[0].re.ReplaceAllString(s, patterns[0].repl)
	s = keyValue.ReplaceAllStringFunc(s, func(m string) string {
		parts := keyValue.FindStringSubmatch(m)
		if notSecrets[strings.ToLower(parts[2])] {
			return m
		}
		return parts[1] + redacted
	})
	for _, p := range patterns[1:] {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

// RedactingWriter passes output through line by line with secrets hidden, for streaming a
// tool's output live. A private key spans lines, so its lines are dropped until its end
// marker. Call Flush when the stream ends to write a last line without a newline.
type RedactingWriter struct {
	w     io.Writer
	buf   []byte
	inKey bool
}

func NewRedactingWriter(w io.Writer) *RedactingWriter { return &RedactingWriter{w: w} }

func (r *RedactingWriter) Write(p []byte) (int, error) {
	r.buf = append(r.buf, p...)
	for {
		i := bytes.IndexByte(r.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := string(r.buf[:i+1])
		r.buf = r.buf[i+1:]
		if err := r.writeLine(line); err != nil {
			return len(p), err
		}
	}
}

func (r *RedactingWriter) Flush() error {
	if len(r.buf) == 0 {
		return nil
	}
	line := string(r.buf)
	r.buf = nil
	return r.writeLine(line)
}

func (r *RedactingWriter) writeLine(line string) error {
	switch {
	case strings.Contains(line, "PRIVATE KEY-----") && strings.Contains(line, "-----BEGIN") && !strings.Contains(line, "-----END"):
		r.inKey = true
		_, err := io.WriteString(r.w, redacted+"\n")
		return err
	case r.inKey:
		if strings.Contains(line, "-----END") {
			r.inKey = false
		}
		return nil
	}
	_, err := io.WriteString(r.w, Redact(line))
	return err
}
