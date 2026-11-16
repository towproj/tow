---
title: Installing tow
linkTitle: Install
weight: 10
---
# Installing tow

tow runs on your machine (Linux or macOS, amd64 or arm64) and reaches the cluster's machines over SSH and their providers' APIs.

## Debian, Ubuntu and their derivatives (apt)
```bash
sudo mkdir -p /etc/apt/keyrings
sudo curl -fsSLo /etc/apt/keyrings/tow.asc https://towproj.github.io/tow/apt/gpg.key
echo "deb [signed-by=/etc/apt/keyrings/tow.asc] https://towproj.github.io/tow/apt stable main" | sudo tee /etc/apt/sources.list.d/tow.list
sudo apt update && sudo apt install tow
```
`sudo apt upgrade` brings later releases.

## RHEL, Rocky Linux, AlmaLinux, Fedora (dnf)
```bash
sudo curl -fsSLo /etc/yum.repos.d/tow.repo https://towproj.github.io/tow/rpm/tow.repo
sudo dnf install tow
```
dnf checks the repository's signature and each package's. `sudo dnf upgrade` brings later releases.

## macOS (Homebrew)
```bash
brew install --cask towproj/tap/tow
```

## Any Linux or macOS: the binary
Download the archive for your system from [the releases](https://github.com/towproj/tow/releases), check it, and put `tow` on your PATH:
```bash
tar -xzf tow_0.1.0_linux_amd64.tar.gz tow
sudo install tow /usr/local/bin/
```
How to check a download's signature and provenance: [releasing.md](releasing.md#checking-a-download).

## With Go
```bash
go install github.com/towproj/tow@latest
```

## Shell completions
The packages and Homebrew install them for bash, zsh and fish. Otherwise: `tow completion --help`.

## What tow needs besides
- **OpenTofu** for machines on AWS, GCP, Azure or Proxmox, and **helm** and **kubectl** for add-ons: when one is missing, tow asks before installing the release it was tested with into `~/.local/bin`.
- **Docker** and **k3d** for a local cluster.
- The provider's credentials: [platforms.md](platforms.md).
