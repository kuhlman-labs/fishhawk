#!/usr/bin/env python3
"""Offline deployment contracts; no Azure credentials or resource mutations."""
import base64
import contextlib
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import yaml

sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[2]


def module(name):
    spec = importlib.util.spec_from_file_location(name, ROOT / 'deploy/azure' / (name + '.py'))
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


CONFIGURE = module('configure')
DEPLOY = module('deploy')
SHA = 'a' * 40
DIGEST = 'sha256:' + 'b' * 64


class NamespaceBoundaries(unittest.TestCase):
    def test_namespaces_cannot_change_boundaries_or_reach_other_databases(self):
        for env in CONFIGURE.ENVIRONMENTS:
            docs = list(CONFIGURE.namespace_documents(env))
            by_kind = {doc['kind']: doc for doc in docs}
            self.assertEqual(by_kind['Namespace']['metadata']['labels']['pod-security.kubernetes.io/enforce'], 'restricted')
            self.assertEqual(by_kind['ResourceQuota']['spec']['hard']['services.loadbalancers'], '0')
            rules = by_kind['Role']['rules']
            resources = {resource for rule in rules for resource in rule['resources']}
            self.assertFalse(resources & {'namespaces', 'networkpolicies', 'roles', 'rolebindings', 'serviceaccounts', 'pods/exec'})
            for rule in rules:
                self.assertNotIn('*', rule['verbs'])
            policy = by_kind['NetworkPolicy']['spec']
            self.assertEqual(set(policy['policyTypes']), {'Ingress', 'Egress'})
            postgres = policy['egress'][1]['to'][0]['ipBlock']['cidr']
            self.assertEqual(postgres, '10.42.3.0/24' if env == 'prod' else '10.42.2.0/24')
            self.assertIn('169.254.0.0/16', policy['egress'][2]['to'][0]['ipBlock']['except'])
            self.assertEqual(by_kind['RoleBinding']['subjects'][0]['namespace'], 'fishhawk-' + env)


class BootstrapSelection(unittest.TestCase):
    def test_dev_bootstrap_never_reads_or_rotates_other_environments(self):
        urls = []
        documents = []

        def urlopen(request, timeout):
            url = request.full_url
            urls.append(url)
            if url.startswith('http://169.254.169.254/'):
                return io.BytesIO(json.dumps({'access_token': 'test-only-identity'}).encode())
            self.assertTrue(url.startswith('https://fishhawkpl-dev.vault.azure.net/secrets/'), url)
            name = url.split('/secrets/')[1].split('?')[0]
            value = {'database-password': 'x' * 32,
                     'database-admin-url': 'postgres://fishhawkadmin:test-only@nonprod.example/postgres'}.get(name, 'test-only')
            return io.BytesIO(json.dumps({'value': value}).encode())

        def kubectl(*args, data=None):
            if args == ('apply', '-f', '-'):
                documents.append(json.loads(data))
            if args == ('-n', 'fishhawk-dev', 'get', 'secret', 'deploy-token', '-o', 'json'):
                return json.dumps({'data': {'ca.crt': 'test-only-ca',
                    'token': base64.b64encode(b'test-only-token').decode()}}).encode()
            return b''

        output = io.StringIO()
        with mock.patch.object(CONFIGURE.urllib.request, 'urlopen', side_effect=urlopen), \
                mock.patch.object(CONFIGURE, 'kubectl', side_effect=kubectl) as kube, \
                mock.patch.object(CONFIGURE.subprocess, 'run') as postgres, contextlib.redirect_stdout(output):
            CONFIGURE.main(['fishhawkpl', '192.0.2.1', 'operator@example.com', '--environment', 'dev'])
        self.assertEqual(set(json.loads(output.getvalue())), {'dev'})
        self.assertEqual({d['metadata']['namespace'] for d in documents if 'namespace' in d['metadata']}, {'fishhawk-dev'})
        self.assertEqual([d['metadata']['name'] for d in documents if d['kind'] == 'Namespace'], ['fishhawk-dev'])
        deletes = [call.args for call in kube.call_args_list if 'delete' in call.args]
        self.assertEqual(deletes, [('-n', 'fishhawk-dev', 'delete', 'secret', 'deploy-token', '--ignore-not-found')])
        postgres.assert_called_once()
        self.assertEqual(postgres.call_args.kwargs['env']['PGHOST'], 'nonprod.example')
        self.assertTrue(any('/secrets/github-app-private-key?' in url for url in urls))

    def test_unknown_environment_fails_before_any_azure_or_cluster_access(self):
        with mock.patch.object(CONFIGURE.urllib.request, 'urlopen') as azure, \
                mock.patch.object(CONFIGURE, 'kubectl') as kube, contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit) as failure:
                CONFIGURE.main(['fishhawkpl', '192.0.2.1', 'operator@example.com', '--environment', 'production'])
        self.assertEqual(failure.exception.code, 2)
        azure.assert_not_called()
        kube.assert_not_called()


class Chart(unittest.TestCase):
    def render(self, *extra):
        result = subprocess.run(['helm', 'template', 'fishhawk', str(ROOT / 'deploy/helm/fishhawk'),
            '-f', str(ROOT / 'deploy/azure/values.yaml'), '--namespace', 'fishhawk-prod',
            '--set', 'ingress.host=app.example.com', '--set', 'config.githubAppId=123',
            '--set', 'config.oauthClientId=client', '--set', 'image.digest=' + DIGEST,
            '--set', 'frontend.image=ghcr.io/kuhlman-labs/fishhawk-web@' + DIGEST, *extra],
            capture_output=True, text=True)
        return result

    def test_hosted_chart_has_safe_rollouts_private_credentials_and_shared_origin(self):
        rendered = self.render()
        self.assertEqual(rendered.returncode, 0, rendered.stderr)
        docs = list(yaml.safe_load_all(rendered.stdout))
        workloads = [d for d in docs if d and d['kind'] in {'Deployment', 'Job'}]
        self.assertEqual(len(workloads), 3)
        for doc in workloads:
            spec = doc['spec']['template']['spec']
            self.assertFalse(spec['automountServiceAccountToken'])
            self.assertTrue(spec['securityContext']['runAsNonRoot'])
            for container in spec['containers']:
                self.assertIn('@sha256:', container['image'])
                self.assertFalse(container['securityContext']['allowPrivilegeEscalation'])
                self.assertEqual(container['securityContext']['capabilities']['drop'], ['ALL'])
        server = next(d for d in workloads if d['metadata']['name'] == 'fishhawk')
        self.assertEqual(server['spec']['strategy']['type'], 'Recreate')
        pod = server['spec']['template']['spec']
        self.assertEqual(pod['securityContext']['fsGroup'], 65532)
        self.assertIn({'name': 'scratch', 'mountPath': '/tmp'}, pod['containers'][0]['volumeMounts'])
        self.assertIn({'name': 'scratch', 'emptyDir': {'sizeLimit': '512Mi'}}, pod['volumes'])
        job = next(d for d in workloads if d['kind'] == 'Job')
        self.assertEqual(job['metadata']['annotations']['helm.sh/hook'], 'pre-install,pre-upgrade')
        migration_env = job['spec']['template']['spec']['containers'][0]['envFrom']
        self.assertTrue(next(ref['configMapRef'] for ref in migration_env if 'configMapRef' in ref)['optional'])
        self.assertNotIn('optional', next(ref['secretRef'] for ref in migration_env if 'secretRef' in ref))
        ingress = next(d for d in docs if d and d['kind'] == 'Ingress')
        paths = {p['path']: p['backend']['service']['name'] for p in ingress['spec']['rules'][0]['http']['paths']}
        self.assertEqual(paths['/'], 'fishhawk-web')
        self.assertEqual(paths['/v0'], 'fishhawk')
        config = next(d for d in docs if d and d['kind'] == 'ConfigMap')['data']
        self.assertEqual(config['FISHHAWKD_AZURE_BLOB_CONTAINER'], 'traces')
        self.assertNotIn('FISHHAWKD_AZURE_STORAGE_CONNECTION_STRING', config)
        self.assertEqual(config['FISHHAWKD_OAUTH_CALLBACK_URL'], 'https://app.example.com/v0/auth/github/callback')

    def test_invalid_image_or_ambiguous_storage_refused(self):
        for override in ['image.digest=latest', 'config.s3Bucket=old-bucket',
                         'config.extraEnv.FISHHAWKD_AZURE_STORAGE_CONNECTION_STRING=secret']:
            self.assertNotEqual(self.render('--set', override).returncode, 0, override)


class Promotion(unittest.TestCase):
    def test_bundle_and_receipt_bind_every_deployment_input(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            for name in ['fishhawk.tgz', 'values.yaml']:
                (path / name).write_text(name)
            manifest = {'source_sha': SHA, 'build_run_id': '123',
                'backend': 'ghcr.io/kuhlman-labs/fishhawkd@' + DIGEST,
                'frontend': 'ghcr.io/kuhlman-labs/fishhawk-web@' + DIGEST,
                'files': {name: hashlib.sha256((path / name).read_bytes()).hexdigest() for name in ['fishhawk.tgz', 'values.yaml']}}
            (path / 'manifest.json').write_text(json.dumps(manifest))
            _, digest = DEPLOY.validate_bundle(path, SHA, '123')
            receipt = {'environment': 'staging', 'manifest_sha256': digest, 'build_run_id': '123'}
            DEPLOY.validate_promotion(receipt, digest, '123')
            for key, value in [('environment', 'dev'), ('manifest_sha256', 'different'), ('build_run_id', '124')]:
                with self.assertRaises(ValueError):
                    DEPLOY.validate_promotion({**receipt, key: value}, digest, '123')
            for key, value in [('backend', 'ghcr.io/kuhlman-labs/fishhawkd:main'), ('source_sha', 'b' * 40),
                               ('build_run_id', '124'), ('files', {'../../escape': 'bad'})]:
                (path / 'manifest.json').write_text(json.dumps({**manifest, key: value}))
                with self.assertRaises(ValueError):
                    DEPLOY.validate_bundle(path, SHA, '123')
            (path / 'manifest.json').write_text(json.dumps(manifest))
            (path / 'values.yaml').write_text('tampered')
            with self.assertRaises(ValueError):
                DEPLOY.validate_bundle(path, SHA, '123')

    def test_azure_login_is_downstream_of_environment_protection(self):
        for filename, job in [('infra-apply.yml', 'apply'), ('hosted-deploy.yml', 'deploy')]:
            data = yaml.load((ROOT / '.github/workflows' / filename).read_text(), Loader=yaml.BaseLoader)
            jobs = data['jobs']
            self.assertIn('protections', jobs[job]['needs'])
            self.assertNotIn('id-token', jobs['protections']['permissions'])
            self.assertEqual(jobs[job]['permissions']['id-token'], 'write')
            self.assertEqual(jobs[job]['concurrency']['cancel-in-progress'], 'false')
        data = yaml.load((ROOT / '.github/workflows/infra-apply.yml').read_text(), Loader=yaml.BaseLoader)
        self.assertNotIn('push', data['on'])
        self.assertEqual(data['jobs']['apply']['environment'], 'infra')


class DeploymentFailurePaths(unittest.TestCase):
    def exercise(self, health_ok, installed=True):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            bundle = root / 'release-bundle'
            bundle.mkdir()
            for name in ('fishhawk.tgz', 'values.yaml'):
                (bundle / name).write_text(name)
            images = {'backend': 'ghcr.io/kuhlman-labs/fishhawkd@' + DIGEST,
                      'frontend': 'ghcr.io/kuhlman-labs/fishhawk-web@' + DIGEST}
            manifest = {**images, 'source_sha': SHA, 'build_run_id': '123',
                        'files': {name: hashlib.sha256((bundle / name).read_bytes()).hexdigest()
                                  for name in ('fishhawk.tgz', 'values.yaml')}}
            (bundle / 'manifest.json').write_text(json.dumps(manifest))
            calls = []

            def fake_run(*args, check=True, input=None):
                calls.append(args)
                if args[:2] == ('helm', 'upgrade'):
                    self.assertIn(('-f', '-'), list(zip(args, args[1:])))
                    self.assertEqual(json.loads(input)['config'], {'githubAppId': '123', 'oauthClientId': 'client'})
                if args[:2] == ('helm', 'history'):
                    return subprocess.CompletedProcess(args, 0 if installed else 1,
                        json.dumps([{'revision': 3, 'status': 'deployed'}]) if installed else '',
                        '' if installed else 'release: not found')
                if args[0] == 'curl':
                    return subprocess.CompletedProcess(args, 0, json.dumps({'git_sha': SHA if health_ok else 'wrong'}), '')
                if args[0] == 'kubectl' and 'pods' in args:
                    return subprocess.CompletedProcess(args, 0, json.dumps({'items': [
                        {'status': {'phase': 'Running'}, 'spec': {'containers': [{'image': image}]}}
                        for image in images.values()]}), '')
                return subprocess.CompletedProcess(args, 0, '', '')

            import os
            previous = os.getcwd()
            try:
                os.chdir(root)
                with mock.patch.dict(os.environ, {'DEPLOY_ENV': 'staging', 'APP_HOST': 'staging.example.com',
                        'FISHHAWK_APP_ID': '123', 'OAUTH_CLIENT_ID': 'client', 'SOURCE_SHA': SHA, 'BUILD_RUN_ID': '123'}), \
                        mock.patch.object(DEPLOY, 'run', side_effect=fake_run):
                    if health_ok:
                        DEPLOY.main()
                        self.assertEqual(json.loads((root / 'receipt.json').read_text())['environment'], 'staging')
                    else:
                        with self.assertRaisesRegex(ValueError, 'wrong source commit'):
                            DEPLOY.main()
                        self.assertFalse((root / 'receipt.json').exists())
            finally:
                os.chdir(previous)
            self.assertFalse((root / 'release-values.json').exists())
            return calls

    def test_smoke_failure_rolls_back_to_previous_deployment(self):
        calls = self.exercise(False)
        self.assertTrue(any(args[:4] == ('helm', 'rollback', 'fishhawk', '3') for args in calls))

    def test_first_install_failure_does_not_invent_a_previous_revision(self):
        calls = self.exercise(False, installed=False)
        upgrade = next(args for args in calls if args[:2] == ('helm', 'upgrade'))
        self.assertNotIn('--rollback-on-failure', upgrade)
        self.assertFalse(any(args[:2] == ('helm', 'rollback') for args in calls))

    def test_success_records_receipt_without_rollback(self):
        calls = self.exercise(True)
        self.assertFalse(any(args[:2] == ('helm', 'rollback') for args in calls))


if __name__ == '__main__':
    unittest.main()
