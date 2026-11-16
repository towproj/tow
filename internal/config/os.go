package config

import (
	"fmt"
	"regexp"
	"slices"
)

// Operating systems tow puts on the machines it creates on a cloud (spec.infrastructure.os).
// Proxmox VMs run their template's, and your own machines (byoh) theirs: tow reads each
// machine's /etc/os-release and prepares it with that system's own tools.
const (
	OSUbuntu2404  = "ubuntu-24.04"
	OSAlmaLinux9  = "almalinux-9"
	OSAlmaLinux10 = "almalinux-10"

	DefaultOS = OSUbuntu2404
)

// osImages are the systems spec.infrastructure.os offers, with the login user their cloud
// images ship (the default SSH user). AlmaLinux's AMIs are free, with no Marketplace
// subscription; Rocky's need one.
var osImages = map[string]struct{ user string }{
	OSUbuntu2404:  {user: "ubuntu"},
	OSAlmaLinux9:  {user: "ec2-user"},
	OSAlmaLinux10: {user: "ec2-user"},
}

func osNames() []string {
	var out []string
	for name := range osImages {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// ImageUser is the login user an operating system's AWS images ship.
func ImageUser(os string) string {
	if img, ok := osImages[os]; ok {
		return img.user
	}
	return DefaultSSHUser
}

// LoginUser is the default SSH user of the machines tow creates on a public cloud. AWS's
// images ship theirs (ImageUser). On GCP and Azure the platform creates whichever user the SSH
// key names: ubuntu on Ubuntu, almalinux on AlmaLinux, as their own cloud images call it.
func LoginUser(provider, os string) string {
	switch {
	case provider == ProviderAWS:
		return ImageUser(os)
	case RHELFamily(os):
		return "almalinux"
	}
	return DefaultSSHUser
}

// RHELFamily reports whether spec.infrastructure.os is one of the RHEL family (AlmaLinux).
func RHELFamily(os string) bool { return os == OSAlmaLinux9 || os == OSAlmaLinux10 }

// DefaultProxmoxCPUType is the CPU model Proxmox VMs get: what Proxmox itself picks for new
// VMs, which runs on any x86-64 server from the last decade. RHEL 10 and its rebuilds need
// x86-64-v3 (or host).
const DefaultProxmoxCPUType = "x86-64-v2-AES"

var cpuTypePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validateOS checks spec.infrastructure.os: only on public clouds, one of the systems tow
// offers.
func (c *Config) validateOS() []error {
	os := c.Spec.Infrastructure.OS
	if os == "" {
		return nil
	}
	var errs []error
	switch c.Provider() {
	case ProviderAWS, ProviderGCP, ProviderAzure:
	case ProviderProxmox:
		return []error{fmt.Errorf("spec.infrastructure.os isn't used on proxmox: the VMs run their template's system (spec.infrastructure.proxmox.template)")}
	case ProviderBYOH:
		return []error{fmt.Errorf("spec.infrastructure.os isn't used with byoh: your machines already have theirs, and tow prepares each one with its own tools")}
	default:
		return []error{fmt.Errorf("spec.infrastructure.os isn't used by local clusters (k3d runs in Docker on this machine)")}
	}
	if _, ok := osImages[os]; !ok {
		errs = append(errs, fmt.Errorf("spec.infrastructure.os %q is not offered (%s)", os, joinOr(osNames())))
	}
	return errs
}

// validateCPUType checks spec.infrastructure.proxmox.cpuType looks like a Proxmox CPU model;
// Proxmox itself says when it doesn't know one.
func (c *Config) validateCPUType() []error {
	p := c.Spec.Infrastructure.Proxmox
	if p == nil || p.CPUType == "" || cpuTypePattern.MatchString(p.CPUType) {
		return nil
	}
	return []error{fmt.Errorf("spec.infrastructure.proxmox.cpuType %q must be a Proxmox CPU model, like %s, x86-64-v3 or host", p.CPUType, DefaultProxmoxCPUType)}
}
