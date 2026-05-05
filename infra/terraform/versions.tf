# Pinned per ADR-016 (#165). Floor matches what current macOS
# Homebrew ships; bump deliberately when newer features land.
terraform {
  required_version = "~> 1.5"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.50"
    }
  }
}
