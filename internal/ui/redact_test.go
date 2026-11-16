package ui

import (
	"strings"
	"testing"
)

func TestRedactHidesSecrets(t *testing.T) {
	tests := map[string]string{
		"aws key id":         "using AKIAIOSFODNN7EXAMPLE for this",
		"session key id":     "ASIAIOSFODNN7EXAMPLE",
		"key=value":          "aws_secret_access_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"key: value":         "password: hunter2hunter2",
		"word-like password": "password = requiredness", // only exact non-secret values are kept
		"json":               `{"client_secret": "s3cr3t-v4lue"}`,
		"token flag":         "--token=abc123def456", // gitleaks:allow (fake)
		"bearer header":      "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig",
		"k3s join token":     "K10831ab6f0e0c2e3c1f4b5a6978d0e1f2::server:5f0e0c2e3c1f4b5a",
		"kubeadm token":      "kubeadm join 10.0.0.1:6443 --discovery-token-ca-cert-hash x abcdef.0123456789abcdef",
		"private key in one": "key:\n-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n-----END OPENSSH PRIVATE KEY-----\ndone", // gitleaks:allow (fake)
		"kubeconfig key":     "    client-key-data: ZmFrZS1rdWJlY29uZmlnLWtleQ==",                                                            // gitleaks:allow (fake)
		"kubeconfig as json": `{"client-key-data":"ZmFrZS1rdWJlY29uZmlnLWtleQ=="}`,                                                           // gitleaks:allow (fake)
	}
	secrets := []string{"AKIAIOSFODNN7EXAMPLE", "ASIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI", "hunter2", "requiredness", "s3cr3t", "abc123def456",
		"eyJhbGci", "K10831ab", "abcdef.0123456789abcdef", "b3BlbnNzaC1rZXktdjEAAAAA", "ZmFrZS1rdWJlY29uZmlnLWtleQ"}

	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			out := Redact(in)
			if !strings.Contains(out, redacted) {
				t.Errorf("nothing redacted in %q", out)
			}
			for _, s := range secrets {
				if strings.Contains(out, s) {
					t.Errorf("%q still visible in %q", s, out)
				}
			}
		})
	}
}

// Ordinary output must survive untouched, or verbose logs become useless.
func TestRedactKeepsOrdinaryText(t *testing.T) {
	for _, s := range []string{
		"exec: /home/x/.local/bin/tofu apply -input=false -no-color tow.tfplan (in /home/x/.tow/clusters/lab/tofu)",
		"Using arn:aws:iam::123456789012:user/anass",
		"ssh_public_key = ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl test",
		"node ip-10-0-1-5.eu-west-3.compute.internal (10.0.1.5) is ready",
		"https://registry.k8s.io and https://pkgs.k8s.io reached",
		"refusing provision.secretAccessKey, which looks like a credential.",
		"ec2-13-38-1-2.eu-west-3.compute.amazonaws.com:6443",
		// OpenTofu plan fields named like credentials, holding values that never are.
		`      + get_password_data                    = false`,
		`      + password_data                        = (known after apply)`,
		`          + http_tokens                 = "required"`,
		`      + secret_string = (sensitive value)`,
		`token: null`,
	} {
		if got := Redact(s); got != s {
			t.Errorf("changed ordinary text:\n in: %s\nout: %s", s, got)
		}
	}
}

func TestRegisterSecret(t *testing.T) {
	RegisterSecret("generated-join-value-42")
	RegisterSecret("short") // ignored: would blank out ordinary words
	got := Redact("joining with generated-join-value-42, short and sweet")
	if strings.Contains(got, "generated-join-value-42") || !strings.Contains(got, "short and sweet") {
		t.Errorf("got %q", got)
	}
}

func TestRedactingWriterAcrossWrites(t *testing.T) {
	var b strings.Builder
	w := NewRedactingWriter(&b)
	// OpenTofu writes in arbitrary chunks; a secret split across two writes must still be caught.
	for _, chunk := range []string{"aws_access_key_id = AKIAIOSF", "ODNN7EXAMPLE\nok line\n-----BEGIN RSA PRIVATE KEY-----\n", "MIIEpAIBAAKCAQEA\n-----END RSA PRIVATE KEY-----\n", "tail without newline"} { // gitleaks:allow (fake)
		w.Write([]byte(chunk))
	}
	w.Flush()
	out := b.String()
	for _, leaked := range []string{"AKIAIOSFODNN7EXAMPLE", "MIIEpAIBAAKCAQEA"} {
		if strings.Contains(out, leaked) {
			t.Errorf("%q leaked:\n%s", leaked, out)
		}
	}
	for _, kept := range []string{"ok line\n", "tail without newline"} {
		if !strings.Contains(out, kept) {
			t.Errorf("%q missing:\n%s", kept, out)
		}
	}
}
