#!/usr/bin/env python3
"""Deploy a verified immutable bundle using namespace-scoped credentials."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys


def validate_bundle(directory, source_sha, build_run_id):
    directory = Path(directory)
    raw = (directory / "manifest.json").read_bytes()
    manifest = json.loads(raw)
    if not re.fullmatch(r"[a-f0-9]{40}", source_sha) or manifest["source_sha"] != source_sha:
        raise ValueError("release source SHA does not match the successful CI run")
    if manifest["build_run_id"] != str(build_run_id):
        raise ValueError("release came from a different CI run")
    for name, repository in (("backend", "fishhawkd"), ("frontend", "fishhawk-web")):
        if not re.fullmatch(r"ghcr.io/kuhlman-labs/" + repository + r"@sha256:[a-f0-9]{64}", manifest[name]):
            raise ValueError("release requires the expected repository and immutable digest")
    if set(manifest["files"]) != {"fishhawk.tgz", "values.yaml"}:
        raise ValueError("unexpected release file set")
    for name, digest in manifest["files"].items():
        if hashlib.sha256((directory / name).read_bytes()).hexdigest() != digest:
            raise ValueError("release file digest mismatch: " + name)
    return manifest, hashlib.sha256(raw).hexdigest()


def validate_promotion(receipt, digest, run_id):
    if receipt.get("environment") != "staging" or receipt.get("manifest_sha256") != digest or receipt.get("build_run_id") != str(run_id):
        raise ValueError("prod requires a successful staging receipt for this exact release bundle")


def run(*args, check=True, input=None):
    return subprocess.run(args, check=check, input=input, capture_output=True, text=True)


def main():
    environment = os.environ["DEPLOY_ENV"]
    if environment not in {"dev", "staging", "prod"}:
        raise ValueError("invalid environment")
    host = os.environ["APP_HOST"]
    if not re.fullmatch(r"[a-z0-9][a-z0-9.-]+[a-z0-9]", host):
        raise ValueError("APP_HOST must be a DNS hostname")
    for key in ("FISHHAWK_APP_ID", "OAUTH_CLIENT_ID"):
        if not os.environ.get(key):
            raise ValueError("missing environment configuration: " + key)
    bundle = Path("release-bundle")
    manifest, digest = validate_bundle(bundle, os.environ["SOURCE_SHA"], os.environ["BUILD_RUN_ID"])
    if environment == "prod":
        validate_promotion(json.loads(Path("staging-receipt/receipt.json").read_text()), digest, os.environ["BUILD_RUN_ID"])
    for image in (manifest["backend"], manifest["frontend"]):
        run("cosign", "verify", image, "--certificate-identity",
            "https://github.com/kuhlman-labs/fishhawk/.github/workflows/backend-build.yml@refs/heads/main",
            "--certificate-oidc-issuer", "https://token.actions.githubusercontent.com")
    namespace = "fishhawk-" + environment
    # App secrets are created by the operator. Refuse a missing Secret before
    # pre-install migration hooks, rather than hang on a missing dependency.
    run("kubectl", "-n", namespace, "get", "secret", "fishhawk-secrets")
    values = {"image": {"digest": manifest["backend"].split("@", 1)[1]},
              "frontend": {"image": manifest["frontend"]}, "ingress": {"host": host},
              "config": {"githubAppId": os.environ["FISHHAWK_APP_ID"],
                         "oauthClientId": os.environ["OAUTH_CLIENT_ID"]}}
    history = run("helm", "history", "fishhawk", "-n", namespace, "-o", "json", check=False)
    previous = None
    if history.returncode == 0:
        successful = [row for row in json.loads(history.stdout) if row["status"] == "deployed"]
        if successful:
            previous = str(successful[-1]["revision"])
    elif "release: not found" not in history.stderr.lower():
        raise RuntimeError("cannot determine existing Helm release; refusing deployment")
    rollback_options = ("--rollback-on-failure",) if previous else ()
    try:
        run("helm", "upgrade", "--install", "fishhawk", str(bundle / "fishhawk.tgz"), "-n", namespace,
            "-f", str(bundle / "values.yaml"), "-f", "-", *rollback_options,
            "--wait", "--wait-for-jobs", "--timeout", "10m", "--history-max", "10",
            input=json.dumps(values))
        # curl retries accommodate first certificate issuance; TLS verification
        # stays on, and the body must identify the exact built commit.
        health = run("curl", "--fail", "--silent", "--show-error", "--retry", "12", "--retry-all-errors",
                     "--retry-delay", "5", "--max-time", "20", "https://" + host + "/healthz")
        if json.loads(health.stdout).get("git_sha") != manifest["source_sha"]:
            raise ValueError("health endpoint is serving the wrong source commit")
        run("curl", "--fail", "--silent", "--show-error", "--max-time", "20", "https://" + host + "/index.html")
        pods = json.loads(run("kubectl", "-n", namespace, "get", "pods", "-o", "json").stdout)
        images = {container["image"] for pod in pods["items"] if pod["status"].get("phase") == "Running"
                  for container in pod["spec"]["containers"]}
        if not {manifest["backend"], manifest["frontend"]}.issubset(images):
            raise ValueError("running pods do not contain both promoted image digests")
    except Exception:
        # Do not emit Secrets or kubeconfig. Workload status/events retain the
        # evidence; operator can inspect migration logs with scoped credentials.
        for resource in ("pods", "events"):
            result = run("kubectl", "-n", namespace, "get", resource, check=False)
            print(result.stdout, file=sys.stderr)
        if previous:
            run("helm", "rollback", "fishhawk", previous, "-n", namespace, "--wait", "--timeout", "10m")
        # First install has no previous version; retain it for diagnosis rather
        # than claim rollback. DB migrations are NEVER automatically reversed.
        raise
    Path("receipt.json").write_text(json.dumps({"environment": environment, "manifest_sha256": digest,
        "build_run_id": os.environ["BUILD_RUN_ID"], "source_sha": manifest["source_sha"]}) + "\n")
    print(f"Deployed {environment}: {manifest['source_sha']} ({digest})")


if __name__ == "__main__":
    main()
