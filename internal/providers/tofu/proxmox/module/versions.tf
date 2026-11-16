# OpenTofu reads the "terraform" block as its own.
terraform {
  required_version = ">= 1.12.0"

  required_providers {
    proxmox = {
      source = "bpg/proxmox"
      # 0.x releases may break between minor versions: move this on purpose, after a test run.
      # The exact release is pinned in .terraform.lock.hcl, from registry.opentofu.org: tofu providers lock
      # -platform=linux_amd64 -platform=linux_arm64 -platform=darwin_amd64 -platform=darwin_arm64
      # (with -upgrade for a newer one).
      version = "~> 0.120.0"
    }
  }
}
