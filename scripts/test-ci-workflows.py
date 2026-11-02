#!/usr/bin/env python3
"""Regression contracts for CI routing, publication authority and deployment guards.

Run with PyYAML 6.0.3 (CI installs the exact version in a disposable venv).
These checks intentionally span workflow boundaries that actionlint cannot
prove: required rollup coverage, trigger inputs, tested artifact handoff and
GitHub's queue property (not yet recognized by actionlint 1.7.12).
"""
import copy
import fnmatch
import importlib.util
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import yaml

sys.dont_write_bytecode = True

ROOT = Path(__file__).resolve().parent.parent


def workflow(name):
    return yaml.load((ROOT / ".github/workflows" / name).read_text(), Loader=yaml.BaseLoader)


CI = workflow("ci.yml")
SPEC = importlib.util.spec_from_file_location("deploy_guard", ROOT / "scripts/check-deploy-environment.py")
GUARD = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GUARD)


class WorkflowContracts(unittest.TestCase):
    def test_validation_is_required_and_publication_is_downstream(self):
        self.assertIn("github.run_id", CI["concurrency"]["group"])
        self.assertIn("pull_request", CI["concurrency"]["group"])
        jobs = CI["jobs"]
        validators = set(jobs) - {"ci-pass", "publish-backend", "publish-gate"}
        self.assertEqual(set(jobs["ci-pass"]["needs"]), validators)
        self.assertNotIn("if", jobs["repository-checks"])
        commands = "\n".join(step.get("run", "") for step in jobs["repository-checks"]["steps"])
        self.assertIn("scripts/test verify --no-tests", commands)
        self.assertIn("command -v zsh", commands)
        self.assertIn('scripts/check-review-prompt-eval --base "$BASE_SHA"', commands)
        for job in ["publish-backend", "publish-gate"]:
            self.assertIn("ci-pass", jobs[job]["needs"])
            self.assertIn("needs.ci-pass.result == 'success'", jobs[job]["if"])
            self.assertIn("github.event_name == 'push'", jobs[job]["if"])
            self.assertIn("github.ref == 'refs/heads/main'", jobs[job]["if"])

    def test_non_source_inputs_do_not_bypass_relevant_checks(self):
        filter_step = next(s for s in CI["jobs"]["detect"]["steps"] if s.get("id") == "filter")
        filters = yaml.load(filter_step["with"]["filters"], Loader=yaml.BaseLoader)
        cases = {
            "go": ["backend/internal/spec/schemas/workflow-v2.schema.json",
                   "docs/spec/workflow-v2.schema.json", "backend/internal/postgres/migrations/new.sql",
                   "scripts/sync-schemas", "scripts/test-check-coverage", "docs/api/v0.openapi.yaml",
                   "docs/adr/index.json", ".fishhawk/workflows.yaml", "newmodule/go.mod"],
            "docker": ["pricing/pricing.go", "credstore/store.go", "redaction/redact.go",
                       "directory/directory.go", "backend/Dockerfile", "newmodule/go.mod"],
            "site": ["site/pnpm-lock.yaml", "site/src/content/docs/reference/api.md"],
            "helm": ["deploy/helm/fishhawk/values.yaml", "scripts/test-helm-render"],
            "infra": ["infra/terraform/iam.tf", ".github/workflows/infra-apply.yml"],
            "gate-image": ["deploy/gate-image/Dockerfile", ".github/workflows/gate-image-publish.yml",
                           "frontend/package.json", "frontend/pnpm-lock.yaml",
                           "scripts/check-frontend", "scripts/verify-change"],
        }
        for lane, paths in cases.items():
            for path in paths:
                with self.subTest(lane=lane, path=path):
                    self.assertTrue(any(fnmatch.fnmatchcase(path, glob) for glob in filters[lane]))
        # A routine module dependency bump still gets static pin validation,
        # but must not rebuild both toolchain images.
        self.assertFalse(any(fnmatch.fnmatchcase("backend/go.mod", glob) for glob in filters["gate-image"]))

    def test_test_matrix_matches_partition_authority(self):
        legs = subprocess.check_output([str(ROOT / "scripts/ci-test-leg"), "--list"], text=True).split()
        self.assertEqual(CI["jobs"]["go-test"]["strategy"]["matrix"]["leg"], legs)
        self.assertIn("backend-server", legs)
        self.assertIn("backend-mcp", legs)

    def test_queue_configuration_and_job_timeouts(self):
        for path in (ROOT / ".github/workflows").glob("*.yml"):
            self.assertNotIn("ubuntu-latest", path.read_text(), path.name)
            data = workflow(path.name)
            scopes = [data, *data.get("jobs", {}).values()]
            for scope in scopes:
                concurrency = scope.get("concurrency", {})
                if isinstance(concurrency, dict) and "queue" in concurrency:
                    self.assertIn(concurrency["queue"], ["max", "single"])
                    if concurrency["queue"] == "max":
                        self.assertEqual(concurrency["cancel-in-progress"], "false")
                if "runs-on" in scope:
                    self.assertGreater(int(scope["timeout-minutes"]), 0, path.name)
                    self.assertLessEqual(int(scope["timeout-minutes"]), 120, path.name)
        self.assertEqual(workflow("fishhawk.yml")["concurrency"]["queue"], "max")

    def test_gate_publisher_reuses_tested_archives(self):
        build = workflow("gate-image.yml")["jobs"]["build"]
        self.assertNotIn("permissions", build)
        commands = "\n".join(step.get("run", "") for step in build["steps"])
        self.assertIn('docker load --input "$RUNNER_TEMP/gate-$ARCH.tar"', commands)
        self.assertIn("scripts/check-gate-image --image fishhawk-gate:ci-", commands)
        image_build = next(s for s in build["steps"] if "build-push-action" in s.get("uses", ""))
        self.assertEqual(image_build["with"]["build-contexts"], "frontend=./frontend")
        offline = next(s for s in build["steps"] if s.get("name") == "Offline frontend gate")
        self.assertIn("--network=none", offline["run"])
        self.assertIn("fishhawk-gate:ci-${{ matrix.arch }} scripts/check-frontend", offline["run"])
        archive = next(s for s in build["steps"] if "upload-artifact" in s.get("uses", ""))
        self.assertLess(build["steps"].index(offline), build["steps"].index(archive))
        publisher = workflow("gate-image-publish.yml")
        self.assertEqual(set(publisher["on"]), {"workflow_call"})
        steps = publisher["jobs"]["publish"]["steps"]
        self.assertFalse(any("build-push-action" in step.get("uses", "") for step in steps))
        downloaded = {s["with"]["name"] for s in steps if "download-artifact" in s.get("uses", "")}
        self.assertEqual(downloaded, {"gate-image-amd64", "gate-image-arm64"})

    def test_mutable_image_tags_are_serialized_and_freshness_checked(self):
        for file, job in [("backend-build.yml", "build-and-push"), ("gate-image-publish.yml", "publish")]:
            data = workflow(file)
            self.assertEqual(set(data["on"]), {"workflow_call"})
            publish = data["jobs"][job]
            self.assertEqual(publish["concurrency"]["cancel-in-progress"], "false")
            self.assertEqual(publish["concurrency"]["queue"], "max")
            promote = publish["steps"][-1]["run"]
            self.assertIn('scripts/ci-image-current', promote)
            self.assertIn('git fetch --no-tags origin main', promote)
            self.assertIn('*) exit "$rc"', promote)
            self.assertIn("imagetools create", promote)
        backend = workflow("backend-build.yml")["jobs"]["build-and-push"]
        build = next(s for s in backend["steps"] if s.get("id") == "build")
        self.assertIn("GIT_SHA=${{ github.sha }}", build["with"]["build-args"])

    def test_release_test_jobs_cannot_sign_or_publish(self):
        for component in ["cli", "runner", "mcp", "backend"]:
            data = workflow(f"{component}-release.yml")
            self.assertEqual(data["permissions"], {"contents": "read"})
            for name, job in data["jobs"].items():
                if name != "release":
                    self.assertNotIn("permissions", job)
                    self.assertFalse(any("cosign" in step.get("run", "") for step in job["steps"]))
            release = data["jobs"]["release"]
            self.assertTrue(release["needs"])
            self.assertEqual(release["permissions"]["id-token"], "write")
            for step in release["steps"]:
                self.assertNotIn("go test", step.get("run", ""))
                if "action-gh-release" in step.get("uses", ""):
                    self.assertEqual(step["with"]["fail_on_unmatched_files"], "true")

    def test_azure_credentials_depend_on_protection_and_validation_jobs(self):
        jobs = workflow("infra-apply.yml")["jobs"]
        self.assertEqual(set(jobs["apply"]["needs"]), {"protections", "validate"})
        self.assertNotIn("id-token", jobs["protections"]["permissions"])
        self.assertIn("scripts/check-deploy-environment.py", jobs["protections"]["steps"][-1]["run"])


class ImageFreshness(unittest.TestCase):
    def test_frontend_gate_inputs_prevent_stale_promotion(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            def git(*args):
                return subprocess.check_output(["git", "-c", "user.name=CI test", "-c",
                    "user.email=ci@example.invalid", "-c", "commit.gpgsign=false", *args], cwd=root, text=True).strip()
            git("init", "--quiet")
            git("commit", "--quiet", "--allow-empty", "-m", "base")
            for path in ["frontend/package.json", "frontend/pnpm-lock.yaml",
                         "scripts/check-frontend", "scripts/verify-change"]:
                with self.subTest(path=path):
                    base = git("rev-parse", "HEAD")
                    file = root / path
                    file.parent.mkdir(parents=True, exist_ok=True)
                    file.write_text("changed input")
                    git("add", ".")
                    git("commit", "--quiet", "-m", "change input")
                    result = subprocess.run([str(ROOT / "scripts/ci-image-current"), "gate", base, "HEAD"],
                        cwd=root, capture_output=True, text=True)
                    self.assertEqual(result.returncode, 1, result.stderr)

    def test_unrelated_commits_can_promote_but_changed_inputs_and_unknown_refs_cannot(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            def git(*args):
                return subprocess.check_output(["git", "-c", "user.name=CI test", "-c",
                    "user.email=ci@example.invalid", "-c", "commit.gpgsign=false", *args], cwd=root, text=True).strip()
            git("init", "--quiet")
            (root / "pricing").mkdir()
            (root / "pricing/pricing.go").write_text("original")
            (root / "README.md").write_text("original")
            git("add", ".")
            git("commit", "--quiet", "-m", "base")
            base = git("rev-parse", "HEAD")
            (root / "README.md").write_text("documentation only")
            git("commit", "--quiet", "-am", "docs")
            def check(kind, revision="HEAD"):
                return subprocess.run([str(ROOT / "scripts/ci-image-current"), kind, base, revision],
                    cwd=root, capture_output=True, text=True).returncode
            self.assertEqual(check("backend"), 0)
            self.assertEqual(check("gate"), 0)
            (root / "pricing/pricing.go").write_text("changed pricing")
            git("commit", "--quiet", "-am", "code")
            self.assertEqual(check("backend"), 1)
            self.assertEqual(check("gate"), 0)
            (root / "deploy/gate-image").mkdir(parents=True)
            (root / "deploy/gate-image/Dockerfile").write_text("changed image")
            git("add", ".")
            git("commit", "--quiet", "-m", "image")
            self.assertEqual(check("gate"), 1)
            self.assertEqual(check("backend", "missing-ref"), 2)
            self.assertEqual(check("gate", "--help"), 2)


class DeploymentProtections(unittest.TestCase):
    def setUp(self):
        self.env = {"name": "prod", "can_admins_bypass": False,
                    "deployment_branch_policy": {"custom_branch_policies": True, "protected_branches": False},
                    "protection_rules": [{"type": "required_reviewers", "prevent_self_review": True,
                                          "reviewers": [{"type": "Team", "reviewer": {"id": 1}}]}]}
        self.pages = [{"branch_policies": [{"name": "main", "type": "branch"}]}]

    def test_protected_prod_and_main_only_dev_pass(self):
        GUARD.validate("prod", self.env, self.pages)
        dev = copy.deepcopy(self.env)
        dev.update(name="dev", protection_rules=[])
        GUARD.validate("dev", dev, self.pages)

    def test_missing_or_weakened_prod_protections_fail(self):
        for change in [dict(protection_rules=[]), dict(deployment_branch_policy=None),
                       dict(name="dev"),
                       dict(protection_rules=[{"type": "required_reviewers", "reviewers": []}])]:
            with self.subTest(change=change), self.assertRaises(ValueError):
                GUARD.validate("prod", {**self.env, **change}, self.pages)
        self.env["protection_rules"][0]["prevent_self_review"] = False
        with self.assertRaises(ValueError):
            GUARD.validate("prod", self.env, self.pages)

    def test_additional_policy_on_later_page_is_rejected(self):
        pages = self.pages + [{"branch_policies": [{"name": "*", "type": "branch"}]}]
        with self.assertRaises(ValueError):
            GUARD.validate("prod", self.env, pages)
        with self.assertRaises(ValueError):
            GUARD.validate("prod", self.env, [{"branch_policies": [{"name": "main", "type": "tag"}]}])


if __name__ == "__main__":
    unittest.main()
