#!/bin/sh
# test-package-repos.sh SITE_DIR
#
# Inside a Debian/Ubuntu or RHEL-family container: adds the repositories package-repos.sh
# built (served from SITE_DIR as file:///site), the way a user adds them, installs tow
# with apt or dnf (which check every signature), and checks what was installed.
set -eu
site=$(cd "${1:?site folder, served as file:///site}" && pwd)
[ "$site" = /site ] || ln -sfn "$site" /site

if command -v apt-get >/dev/null; then
  mkdir -p /etc/apt/keyrings
  cp /site/apt/gpg.key /etc/apt/keyrings/tow.asc
  echo "deb [signed-by=/etc/apt/keyrings/tow.asc] file:/site/apt stable main" >/etc/apt/sources.list.d/tow.list
  apt-get update -qq
  apt-get install -y -qq tow
  test -f /usr/share/zsh/vendor-completions/_tow
  # Listed rather than looked for: Ubuntu's container image keeps nothing in /usr/share/doc.
  dpkg -L tow | grep -qx /usr/share/doc/tow/LICENSE
else
  cp /site/rpm/tow.repo /etc/yum.repos.d/
  dnf install -y -q tow
  test -f /usr/share/zsh/site-functions/_tow
  # The package itself is signed, not only the repository.
  rpm -q --qf '%{RSAHEADER:pgpsig}\n' tow | grep -v '(none)'
  rpm -ql tow | grep -qx /usr/share/doc/tow/LICENSE
fi

tow version
tow --version | grep -q '^tow v'
test -f /usr/share/bash-completion/completions/tow
test -f /usr/share/fish/vendor_completions.d/tow.fish
if tow apply -f /nonexistent.yaml; then
  echo "apply with a missing file should fail" >&2
  exit 1
fi
echo "OK: tow installed from the repository and checked"
