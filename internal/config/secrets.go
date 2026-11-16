package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/towproj/tow/internal/ui"
)

// credentialHints match normalized keys (lowercase, no '-' or '_'). tow.yaml is meant to be
// committed, so a key like these is refused by name before decoding: the value never reaches an error message.
var credentialHints = []string{"secret", "password", "passwd", "token", "credential", "apikey", "accesskey", "privatekey"}

const credentialAdvice = "tow.yaml is meant to be committed and must never hold secrets. " +
	"tow uses your cloud's standard credentials instead: a login ('aws sso login', 'az login', " +
	"'gcloud auth application-default login') or, for service accounts, the cloud's usual environment " +
	"variables or key file. Reference SSH keys with ssh.keyPath, never inline them. A flag or a chart " +
	"value that needs a secret names an environment variable instead ({env: VARIABLE} in extraArgs, " +
	"valuesFromEnv in a Helm chart): tow reads it when you run it and hides it in all output"

// rejectCredentials walks the raw document and fails on anything that looks like a secret,
// naming only its path: the value never reaches a message. Three zones, each with its rules:
//   - tow's own fields: a key named like a credential (password, apiToken, ...) is refused;
//   - extraArgs: flag names say little (secrets-encryption is an on/off switch), so only the
//     flags that take a secret inline are refused;
//   - a Helm chart's values: the chart's vocabulary, where many keys name a Secret without
//     holding one (existingSecret, secretName, imagePullSecrets).
//
// Everywhere, a value shaped like a secret (a private key, a cloud key, a join token, a
// "password=..." pair) is refused, whatever its key.
func rejectCredentials(doc *yaml.Node) error {
	var found []string
	var walk func(n *yaml.Node, path string)
	walk = func(n *yaml.Node, path string) {
		switch n.Kind {
		case yaml.DocumentNode:
			for _, c := range n.Content {
				walk(c, path)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				key, value := n.Content[i].Value, n.Content[i+1]
				p := key
				if path != "" {
					p = path + "." + key
				}
				switch {
				case key == "extraArgs" && value.Kind == yaml.MappingNode:
					found = append(found, secretFlagsIn(value, p)...)
				case helmValues.MatchString(p):
					found = append(found, secretsInValues(value, p, false)...)
				case helmValuesFromEnv.MatchString(p):
					// Keys are value paths (auth.password) on purpose; values must be variable names.
					if looksSecret(value) {
						found = append(found, p)
					}
				case looksLikeCredential(key) && !namesLocation(key, value):
					found = append(found, p)
				default:
					walk(value, p)
				}
			}
		case yaml.SequenceNode:
			for i, c := range n.Content {
				walk(c, fmt.Sprintf("%s[%d]", path, i))
			}
		case yaml.ScalarNode:
			if looksSecret(n) {
				found = append(found, path)
			}
		}
	}
	walk(doc, "")

	if len(found) == 0 {
		return nil
	}
	// No "key: ..." shape here: output redaction would read the next word as the key's value.
	return errors.New("refusing " + strings.Join(found, ", ") + ", which looks like a credential. " + credentialAdvice)
}

// secretFlagsIn refuses flags that take a secret (secretFlags) unless they name an
// environment variable, and any value shaped like a secret.
func secretFlagsIn(args *yaml.Node, path string) []string {
	var found []string
	for i := 0; i+1 < len(args.Content); i += 2 {
		name, value := args.Content[i].Value, args.Content[i+1]
		_, fromEnv := envRef(value)
		if looksSecret(value) || (!fromEnv && contains(secretFlags, strings.ReplaceAll(strings.ToLower(name), "_", "-"))) {
			found = append(found, path+"."+name)
		}
	}
	return found
}

var (
	helmValues        = regexp.MustCompile(`^spec\.kubernetes\.addons\.helmCharts\[\d+\]\.values$`)
	helmValuesFromEnv = regexp.MustCompile(`^spec\.kubernetes\.addons\.helmCharts\[\d+\]\.valuesFromEnv$`)
)

// Keys in chart values that refer to a Kubernetes Secret instead of holding one.
var (
	secretRefPrefixes = []string{"existing"}                                               // existingSecret, existingSecretPasswordKey
	secretRefSuffixes = []string{"secretname", "secretref", "secretkeyref", "pullsecrets"} // tls.secretName, envFrom.secretRef, valueFrom.secretKeyRef, imagePullSecrets
	// Under a secret-named key, only these structural fields may hold plain text: the rest
	// would be the secret itself (secret: {value: ...}).
	secretStructure = []string{"name", "key", "create", "enabled", "optional", "namespace"}
)

// secretsInValues checks a chart's values. inSecret is true below a key named like a credential.
func secretsInValues(n *yaml.Node, path string, inSecret bool) []string {
	var found []string
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, value := n.Content[i].Value, n.Content[i+1]
			p := path + "." + key
			secretNamed := looksLikeCredential(key) && !refersToSecret(key) && !namesLocation(key, value)
			if value.Kind != yaml.ScalarNode {
				found = append(found, secretsInValues(value, p, inSecret || secretNamed)...)
				continue
			}
			plain := !secretNamed && (!inSecret || contains(secretStructure, normalizeKey(key)) || refersToSecret(key) || namesLocation(key, value))
			if looksSecret(value) || (!plain && !isBool(value) && value.Value != "") {
				found = append(found, p)
			}
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			p := fmt.Sprintf("%s[%d]", path, i)
			if c.Kind == yaml.ScalarNode && (looksSecret(c) || (inSecret && !isBool(c))) {
				found = append(found, p)
				continue
			}
			found = append(found, secretsInValues(c, p, inSecret)...)
		}
	case yaml.ScalarNode:
		if looksSecret(n) {
			found = append(found, path)
		}
	}
	return found
}

func refersToSecret(key string) bool {
	k := normalizeKey(key)
	for _, p := range secretRefPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	for _, s := range secretRefSuffixes {
		if strings.HasSuffix(k, s) {
			return true
		}
	}
	return false
}

// isBool spots on/off switches (secret: {create: true}): never a secret.
func isBool(n *yaml.Node) bool { return n.Kind == yaml.ScalarNode && n.ShortTag() == "!!bool" }

// looksSecret matches values the output redaction would hide: keys, tokens, key=secret pairs.
func looksSecret(n *yaml.Node) bool {
	if n.Kind == yaml.ScalarNode {
		return ui.Redact(n.Value) != n.Value
	}
	for _, c := range n.Content {
		if looksSecret(c) {
			return true
		}
	}
	return false
}

// locationSuffixes end keys that say where a secret is kept rather than holding it: an
// environment variable (passwordEnv) or a file (tls-private-key-file, privateKeyPath).
var locationSuffixes = []string{"env", "file", "path"}

// namesLocation accepts such a key when its value is a single line: an inline key or
// password under one of these names is still refused.
func namesLocation(key string, value *yaml.Node) bool {
	k := normalizeKey(key)
	for _, s := range locationSuffixes {
		if strings.HasSuffix(k, s) {
			return value.Kind == yaml.ScalarNode && !strings.Contains(value.Value, "\n") && !strings.Contains(value.Value, "-----BEGIN")
		}
	}
	return false
}

func normalizeKey(key string) string {
	return strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(key))
}

func looksLikeCredential(key string) bool {
	k := normalizeKey(key)
	for _, h := range credentialHints {
		if strings.Contains(k, h) {
			return true
		}
	}
	return false
}
