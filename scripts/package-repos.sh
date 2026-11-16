#!/usr/bin/env bash
# package-repos.sh PACKAGES_DIR SITE_DIR BASE_URL
#
# Builds the apt and yum repositories from the .deb and .rpm files in PACKAGES_DIR, into
# SITE_DIR/apt and SITE_DIR/rpm, signed with the GPG key GPG_KEY_ID names (in the keyring).
# BASE_URL is where SITE_DIR is served: https://... for the release, file:///... for tests.
# Needs gpg, apt-ftparchive (apt-utils) and createrepo_c. The .rpm files are already signed
# (GoReleaser signs them); this signs the repositories' metadata.
#
# Users then add:
#   apt: deb [signed-by=/etc/apt/keyrings/tow.asc] BASE_URL/apt stable main
#   dnf: BASE_URL/rpm/tow.repo, in /etc/yum.repos.d/
set -euo pipefail

pkgs=${1:?packages folder}
site=${2:?site folder}
base=${3:?base URL}
key=${GPG_KEY_ID:?GPG_KEY_ID names the signing key}
sign() { gpg --batch --yes --local-user "$key" "$@"; }

shopt -s nullglob
debs=("$pkgs"/*.deb)
rpms=("$pkgs"/*.rpm)
if [ ${#debs[@]} -eq 0 ] || [ ${#rpms[@]} -eq 0 ]; then
  echo "no .deb or no .rpm in $pkgs" >&2
  exit 1
fi

# apt: a pool of packages, one suite ("stable"), one component ("main"), two architectures.
apt=$site/apt
mkdir -p "$apt/pool/main"
cp "${debs[@]}" "$apt/pool/main/"
gpg --armor --export "$key" >"$apt/gpg.key"
(
  cd "$apt"
  for arch in amd64 arm64; do
    dir=dists/stable/main/binary-$arch
    mkdir -p "$dir"
    apt-ftparchive --arch "$arch" packages pool/main >"$dir/Packages"
    gzip -9kf "$dir/Packages"
  done
  apt-ftparchive \
    -o APT::FTPArchive::Release::Origin=tow \
    -o APT::FTPArchive::Release::Label=tow \
    -o APT::FTPArchive::Release::Suite=stable \
    -o APT::FTPArchive::Release::Codename=stable \
    -o APT::FTPArchive::Release::Architectures="amd64 arm64" \
    -o APT::FTPArchive::Release::Components=main \
    release dists/stable >Release
  mv Release dists/stable/Release
  sign --clearsign --output dists/stable/InRelease dists/stable/Release
  sign --armor --detach-sign --output dists/stable/Release.gpg dists/stable/Release
)

# yum/dnf: the packages and their metadata, the metadata signed, and the .repo file users add.
rpm=$site/rpm
mkdir -p "$rpm"
cp "${rpms[@]}" "$rpm/"
gpg --armor --export "$key" >"$rpm/gpg.key"
createrepo_c --quiet "$rpm"
sign --armor --detach-sign --output "$rpm/repodata/repomd.xml.asc" "$rpm/repodata/repomd.xml"
cat >"$rpm/tow.repo" <<EOF
[tow]
name=tow
baseurl=$base/rpm
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=$base/rpm/gpg.key
EOF

echo "apt: $(ls "$apt/pool/main" | wc -l) packages; rpm: $(ls "$rpm"/*.rpm | wc -l) packages; in $site"
