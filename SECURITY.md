# Security policy

## Reporting a vulnerability
Please don't open a public issue. Report it privately through GitHub: the repository's **Security** tab → **Report a vulnerability** ([GitHub's guide](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)).

Include what's affected (version, provider, command), how to reproduce it, and what an attacker could do with it. You'll get an answer within 7 days. Once a fix is released, the advisory is published with credit to you, unless you'd rather stay anonymous.

## Supported versions
tow is pre-release (v0.x): fixes go into the latest release only.

## What tow does to stay safe
- Secrets never go in the cluster file: they come from environment variables at run time and are hidden from all output.
- The programs tow downloads itself (containerd, runc, helm, kubectl, OpenTofu) are checked against sha256 sums; OpenTofu's providers against their lock files; k3s by its installer's own sha256 check; packages come from their signed repositories (pkgs.k8s.io, CRI-O's). Helm charts and manifests are pinned by version.
- Every change on your machines is recorded in `/etc/tow/prepared.json` with how to undo it.
- Cloud firewalls let only the admin addresses (your machine by default) reach SSH, the API and NodePorts.
