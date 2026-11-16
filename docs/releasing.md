---
title: Releasing
weight: 100
---
# Releasing

For maintainers. Releases are built by [GoReleaser](https://goreleaser.com) from `.goreleaser.yaml`, in `.github/workflows/release.yml`. How users install them: [install.md](install.md).

## What a release holds
- `tow_<version>_<os>_<arch>.tar.gz` for Linux and macOS, amd64 and arm64: the binary, LICENSE, NOTICE, README, CHANGELOG and shell completions.
- `.deb` and `.rpm` packages: the binary in `/usr/bin`, completions for bash, zsh and fish. Each `.rpm` is signed with the packages' GPG key.
- `checksums.txt`, signed with cosign keyless (`checksums.txt.sigstore.json`); an SBOM per archive; GitHub build provenance for every file.
- The Homebrew cask in `towproj/homebrew-tap`.
- The apt and yum repositories on the project's GitHub Pages site (`/apt`, `/rpm`), rebuilt from scratch from the last five releases by `scripts/package-repos.sh` and signed with the packages' key. Older versions stay in GitHub Releases; nothing accumulates in git, and the site stays far below Pages' 1 GB.

## The site
`.github/workflows/pages.yml` builds the GitHub Pages site: the documentation from `docs/` (Hugo and Docsy, `website/`) at the root, and the repositories under `/apt` and `/rpm`. It runs on every merge to `main` and after every release (the release workflow calls it), and on pull requests it only builds the site and checks its links. After updating Docsy (`hugo mod get github.com/docsy/docsy/theme@<version>` in `website/`), run `hugo mod npm pack` and `npm install` there, and commit `go.mod`, `go.sum`, `package.json`, `package-lock.json` and `packages/`.

## Every pull request
The release workflow builds everything as a snapshot: the `.rpm` files and the repositories are signed with a key made for that run only, and nothing goes to Sigstore or anywhere else. Then it adds the repositories to Ubuntu 24.04, Debian 12, Rocky Linux 9 and AlmaLinux 10 containers the way a user does, and installs tow with apt and dnf, which check every signature (`scripts/test-package-repos.sh`). Pull requests into `main` also build and unit-test tow on macOS.

Locally, build for your own machine only; building every platform at once needs more memory than a small machine has:
```bash
goreleaser check
goreleaser build --single-target --snapshot --clean
./dist/*/tow version
```

## Cutting a release
1. Move the `[Unreleased]` section of CHANGELOG.md under the new version, with the date, in a pull request.
2. Once it's merged, tag `main` and push the tag:
   ```bash
   git tag -s v0.1.0 -m v0.1.0
   git push origin v0.1.0
   ```
3. The workflow publishes the release, then the repositories. A version with a suffix (`v0.2.0-rc.1`) is marked a pre-release and stays out of the repositories.

The release jobs and the site's deployment run only while the repository is public: signatures and provenance name the repository in public logs, and GitHub Pages needs it public. Pages' source must be set to GitHub Actions (Settings → Pages). They need two secrets and a variable:
- `PACKAGES_SIGNING_KEY` (secret): the packages' GPG private key (below).
- `RELEASE_APP_CLIENT_ID` (variable) and `RELEASE_APP_PRIVATE_KEY` (secret): the project's GitHub App, owned by `towproj`, installed on `towproj/homebrew-tap` only, with one permission, Contents: read and write. Each release gets a token from it that is valid for one hour and revoked when the job ends; nothing expires or needs renewing. If the private key is lost or leaked, generate a new one in the App's settings, set the secret again, and delete the old key there.

## The packages' signing key
One key signs the `.rpm` files and both repositories. Users trust it once, when they add a repository, so it must stay the same for years: make it once, keep it safe, and renew it before it expires.
```bash
export GNUPGHOME=$(mktemp -d)        # a keyring of its own, away from yours
gpg --batch --passphrase '' --quick-gen-key 'tow packages <address>' rsa4096 sign 3y
gpg --armor --export-secret-keys 'tow packages' | gh secret set PACKAGES_SIGNING_KEY --repo towproj/tow
gpg --armor --export-secret-keys 'tow packages' > tow-packages.asc   # the offline backup: keep it off this machine
rm -rf "$GNUPGHOME"
```
The key has no passphrase because only the workflow uses it; the secret is what protects it. Never commit it, and never keep a copy in the project folder. Before it expires, extend it from the backup (`gpg --quick-set-expire`), set the secret again, and tell users in the release notes to fetch the public key again (the first command of each install section): their copy still carries the old expiry date.

## Checking a download
```bash
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp 'https://github.com/towproj/tow/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
sha256sum --ignore-missing -c checksums.txt
gh attestation verify tow_0.1.0_linux_amd64.tar.gz --repo towproj/tow
```
