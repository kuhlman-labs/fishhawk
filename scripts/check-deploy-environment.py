#!/usr/bin/env python3
"""Fail closed before OIDC issuance when GitHub environment controls drift.

Input: environment name, GET environment JSON, paginated/slurped GET
 deployment-branch-policies JSON. No credentials or mutations in this program.
"""
import json
import sys


def validate(name, environment, policy_pages):
    if name not in {"dev", "prod"} or environment.get("name") != name:
        raise ValueError("unexpected deployment environment")
    policy = environment.get("deployment_branch_policy") or {}
    if policy.get("custom_branch_policies") is not True or policy.get("protected_branches") is not False:
        raise ValueError("environment must use a custom main-only deployment branch policy")
    policies = [p for page in policy_pages for p in page["branch_policies"]]
    if len(policies) != 1 or policies[0].get("name") != "main" or policies[0].get("type") != "branch":
        raise ValueError("environment must allow only the main branch (no tag or wildcard policies)")
    if name == "prod":
        reviews = [r for r in environment.get("protection_rules", []) if r.get("type") == "required_reviewers"]
        if not reviews or not reviews[0].get("reviewers") or reviews[0].get("prevent_self_review") is not True:
            raise ValueError("prod requires reviewers with self-review disabled")


def main():
    try:
        name, env_file, policies_file = sys.argv[1:]
        with open(env_file, encoding="utf-8") as source:
            environment = json.load(source)
        with open(policies_file, encoding="utf-8") as source:
            pages = json.load(source)
        validate(name, environment, pages)
    except (ValueError, KeyError, TypeError, OSError) as error:
        print(f"::error::Deployment refused: {error}. See infra/terraform/README.md.", file=sys.stderr)
        return 1
    print(f"Deployment environment {name}: protection rules verified")
    return 0


if __name__ == "__main__":
    sys.exit(main())
