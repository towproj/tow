# OpenTofu reads the "terraform" block as its own.
terraform {
  required_version = ">= 1.12.0"

  required_providers {
    # The release tow uses is pinned in .terraform.lock.hcl, from registry.opentofu.org.
    # Move it on purpose, after a test run: tofu providers lock -platform=linux_amd64
    # -platform=linux_arm64 -platform=darwin_amd64 -platform=darwin_arm64 (with -upgrade for a
    # newer one).
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0, < 7.0"
    }
  }
}
