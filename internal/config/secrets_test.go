package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRejectsCredentials(t *testing.T) {
	cases := map[string]struct{ yaml, path string }{
		"aws key in the provider block": {
			yaml: "spec: {infrastructure: {provider: aws, aws: {region: eu-west-3, aws_secret_access_key: wJalrXUtnFEMI}}}",
			path: "spec.infrastructure.aws.aws_secret_access_key",
		},
		"azure client secret": {
			yaml: "spec: {infrastructure: {provider: azure, azure: {region: westeurope, clientSecret: abc}}}",
			path: "spec.infrastructure.azure.clientSecret",
		},
		"gcp credentials file": {
			yaml: "spec: {infrastructure: {provider: gcp, gcp: {region: europe-west1, credentials: '{}'}}}",
			path: "spec.infrastructure.gcp.credentials",
		},
		"proxmox token": {
			yaml: "spec: {infrastructure: {provider: proxmox, proxmox: {apiToken: 'tow@pve!t=hunter2'}}}",
			path: "spec.infrastructure.proxmox.apiToken",
		},
		"ssh password on a machine": {
			yaml: "spec: {controlPlane: {machines: [{host: 10.0.0.1, ssh: {user: root, password: hunter2}}]}}",
			path: "spec.controlPlane.machines[0].ssh.password",
		},
		"inline private key": {
			yaml: "spec: {infrastructure: {access: {ssh: {user: root, private-key: '-----BEGIN'}}}}",
			path: "spec.infrastructure.access.ssh.private-key",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(header + tc.yaml))
			if err == nil {
				t.Fatal("expected credential field to be rejected")
			}
			if !strings.Contains(err.Error(), tc.path) {
				t.Errorf("error should name %s, got: %v", tc.path, err)
			}
			for _, secret := range []string{"wJalrXUtnFEMI", "hunter2", "BEGIN"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error leaks the secret value %q: %v", secret, err)
				}
			}
		})
	}
}

// keyPath points at a key file and is the supported way to configure SSH keys.
func TestKeyPathIsNotACredential(t *testing.T) {
	if looksLikeCredential("keyPath") {
		t.Error("keyPath must stay allowed")
	}
}

// A key that says where a secret is kept (an environment variable, a file) is not the secret.
func TestSecretLocationsAllowed(t *testing.T) {
	for _, spec := range []string{
		"spec: {kubernetes: {storage: {smb: {usernameEnv: SMB_USER, passwordEnv: SMB_PASSWORD}}}}",
		"spec: {kubernetes: {apiServer: {extraArgs: {tls-private-key-file: /etc/k.key, token-auth-file: /etc/tokens.csv}}}}",
		"spec: {x: {privateKeyPath: ~/.ssh/key}}",
	} {
		if err := rejectCredentialsIn(t, spec); err != nil {
			t.Errorf("%s: %v", spec, err)
		}
	}
	// The same names holding the secret itself are still refused, without leaking it.
	err := rejectCredentialsIn(t, "spec: {x: {privateKeyFile: \"-----BEGIN OPENSSH PRIVATE KEY-----\\nabc\"}}")
	if err == nil || strings.Contains(err.Error(), "BEGIN") {
		t.Errorf("an inline key under a file name must be refused without being shown: %v", err)
	}
}

func rejectCredentialsIn(t *testing.T, spec string) error {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(header+spec), &doc); err != nil {
		t.Fatal(err)
	}
	return rejectCredentials(&doc)
}

// In extraArgs a flag's name says little: only flags that take a secret inline, or values
// shaped like one, are refused.
func TestSecretsInExtraArgs(t *testing.T) {
	ok := "spec: {kubernetes: {k3s: {server: {extraArgs: {secrets-encryption: true}}}, apiServer: {extraArgs: {service-account-max-token-expiration: 48h, enable-bootstrap-token-auth: true}}}}"
	if err := rejectCredentialsIn(t, ok); err != nil {
		t.Errorf("ordinary flags refused: %v", err)
	}
	for _, spec := range []string{
		"spec: {kubernetes: {k3s: {server: {extraArgs: {etcd-s3-secret-key: abc}}}}}",
		"spec: {kubernetes: {k3s: {agent: {extraArgs: {token: abc}}}}}",
		"spec: {kubernetes: {apiServer: {extraArgs: {some-flag: K10aaaaaaaaaaaaaaaaaaaaaaaa::server:abcdef}}}}",
		"spec: {kubernetes: {kubelet: {extraArgs: {x: [ok, AKIAABCDEFGHIJKLMNOP]}}}}",
	} {
		err := rejectCredentialsIn(t, spec)
		if err == nil {
			t.Errorf("%s: expected a refusal", spec)
		} else if strings.Contains(err.Error(), "abc") || strings.Contains(err.Error(), "AKIA") {
			t.Errorf("the refusal leaks the value: %v", err)
		}
	}
}

// Chart values name Secrets all the time without holding one; the secret itself is refused.
func TestSecretsInHelmValues(t *testing.T) {
	chart := func(values string) string {
		return "spec: {kubernetes: {addons: {helmCharts: [{name: a, values: " + values + "}]}}}"
	}
	for _, values := range []string{
		"{existingSecret: db-creds, existingSecretPasswordKey: password}",
		"{tls: {secretName: web-tls}, imagePullSecrets: [{name: regcred}]}",
		"{env: [{name: DB, valueFrom: {secretKeyRef: {name: db, key: password}}}], envFrom: [{secretRef: {name: app}}]}",
		"{secret: {create: false, name: shared}, auth: {enabled: true, adminPassword: ''}}",
		"{createSecret: true, crds: {enabled: true}}",
	} {
		if err := rejectCredentialsIn(t, chart(values)); err != nil {
			t.Errorf("%s: %v", values, err)
		}
	}
	for values, path := range map[string]string{
		"{auth: {password: hunter22}}":              "values.auth.password",
		"{secrets: {DB_URL: postgres-hunter22}}":    "values.secrets.DB_URL",
		"{secret: {value: hunter22}}":               "values.secret.value",
		"{passwords: [hunter22]}":                   "values.passwords[0]",
		"{config: {url: 'https://x?token=abc123'}}": "values.config.url",
		"{aws: {keyId: AKIAABCDEFGHIJKLMNOP}}":      "values.aws.keyId",
	} {
		err := rejectCredentialsIn(t, chart(values))
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("%s: expected %s refused, got %v", values, path, err)
		} else if strings.Contains(err.Error(), "hunter22") || strings.Contains(err.Error(), "abc123") || strings.Contains(err.Error(), "AKIA") {
			t.Errorf("the refusal leaks the value: %v", err)
		}
	}
}

// A value shaped like a secret is refused wherever it sits, even under an ordinary key.
func TestSecretShapedValuesAnywhere(t *testing.T) {
	err := rejectCredentialsIn(t, "spec: {kubernetes: {addons: {manifests: ['https://x.example/app.yaml?token=abc123']}}, controlPlane: {labels: {k: AKIAABCDEFGHIJKLMNOP}}}")
	if err == nil || !strings.Contains(err.Error(), "spec.kubernetes.addons.manifests[0]") || !strings.Contains(err.Error(), "spec.controlPlane.labels.k") {
		t.Errorf("expected both values refused, got %v", err)
	}
}
