# Single AWS provider; no aliases until a multi-region story exists.
# Region comes from var.region so the stack can run against us-east-1
# in production and us-east-2 in staging without code changes.
provider "aws" {
  region = var.region

  default_tags {
    tags = {
      Project     = var.project
      Environment = var.environment
      ManagedBy   = "terraform"
      Repo        = var.github_repo
    }
  }
}
