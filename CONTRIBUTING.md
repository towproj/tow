# Contributing to tow

Thanks for your interest. Bug reports, fixes, docs and tested examples are all welcome.

## Before you start
- **A bug:** open an issue with the bug report template: what you ran, what happened, your tow version, provider and system, and the cluster file with any secret removed.
- **A feature or a bigger change:** open an issue first, so we can agree on the design before you write code. tow installs exactly what the cluster file says; a new built-in add-on has to need something a Helm chart can't do (preparing the machines, install-time settings, firewalls, secrets). Otherwise it belongs in `addons.helmCharts`, with a tested example in [docs/](docs/).
- **A security problem:** never in a public issue; see [SECURITY.md](SECURITY.md).

## Building and testing
Go 1.26 or newer.
```bash
go build -o tow     # or: make build
make test               # unit tests; CI runs them with the race detector on every push
make hooks              # once per clone: gitleaks scans every commit for secrets
```
The integration tests create real clusters: k3d on your machine (`make test-integration`, needs Docker and k3d), or machines on AWS, GCP, Azure or Proxmox, which cost a few cents a run. Every test, what it covers, what it costs and how to run it: [docs/testing/README.md](docs/testing/README.md). Run the tests that touch what you changed, and say which ones in your pull request.

## Pull requests
- One change per pull request, with tests beside the code (`*_test.go`) and, for a new field or feature, an example in `examples/`.
- `gofmt`, `go vet` and `make test` pass (CI checks all three).
- No secrets, ever: not in code, tests, examples or cluster files. Anything secret comes from an environment variable at run time.
- Docs follow the code: the README, `docs/` and the command's `--help`. The pages in `docs/` are also the documentation site (Hugo and Docsy, in `website/`); every pull request builds it and checks its links. To preview it, with Hugo extended and Node installed: `cd website && npm ci && npm run hugo -- server`.
- In workflows, actions are pinned to a full commit SHA with the version as a comment (`uses: actions/checkout@<sha> # v7.0.1`), and downloaded tools to a version and its sha256; Dependabot keeps the pins current.
- Pull requests are squash-merged into `main`. How releases are made: [docs/releasing.md](docs/releasing.md).

## Developer Certificate of Origin
Every commit must be signed off, certifying the [Developer Certificate of Origin](https://developercertificate.org/): you wrote the change or have the right to submit it under the project's license (Apache 2.0).
```bash
git commit -s -m "fix(prepare): ..."    # adds "Signed-off-by: Your Name <you@example.com>"
```
Forgot it? `git commit --amend -s`, or `git rebase --signoff main` for several commits.

## Code of Conduct
This project follows the [CNCF Code of Conduct](CODE_OF_CONDUCT.md).
