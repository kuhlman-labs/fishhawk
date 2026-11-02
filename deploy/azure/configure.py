#!/usr/bin/env python3
"""Operator-only, run as root ON the provisioned VM after cloud-init finishes.

Reads app secrets via the VM managed identity, provisions namespace boundaries,
DB owners, TLS, and secrets. Prints ONLY kubeconfigs as JSON to stdout: redirect
into a mode-0600 file, then store each in its matching Key Vault. Never run in CI.
"""
import argparse
import base64
import json
import os
import re
import subprocess
import sys
import urllib.parse
import urllib.request

ENVIRONMENTS = ("dev", "staging", "prod")
CERT_MANAGER = "v1.21.3"


def command(args, data=None):
    return subprocess.check_output(args, input=data, stderr=sys.stderr)


def kubectl(*args, data=None):
    return command(["k3s", "kubectl", *args], data)


def apply(document):
    kubectl("apply", "-f", "-", data=json.dumps(document).encode())


def namespace_documents(env):
    ns = "fishhawk-" + env
    metadata = {"name": ns}
    yield {"apiVersion": "v1", "kind": "Namespace", "metadata": {
        **metadata, "labels": {"pod-security.kubernetes.io/enforce": "restricted",
                                "pod-security.kubernetes.io/enforce-version": "v1.37"}}}
    yield {"apiVersion": "v1", "kind": "ResourceQuota", "metadata": {"name": "budget", "namespace": ns},
           "spec": {"hard": {"requests.cpu": "1", "requests.memory": "1Gi", "limits.cpu": "2",
                             "limits.memory": "2Gi", "pods": "12", "services.loadbalancers": "0", "persistentvolumeclaims": "0"}}}
    yield {"apiVersion": "v1", "kind": "LimitRange", "metadata": {"name": "defaults", "namespace": ns},
           "spec": {"limits": [{"type": "Container", "defaultRequest": {"cpu": "50m", "memory": "64Mi"},
                                "default": {"cpu": "500m", "memory": "512Mi"}}]}}
    yield {"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
           "metadata": {"name": "boundary", "namespace": ns}, "spec": {
               "podSelector": {}, "policyTypes": ["Ingress", "Egress"],
               "ingress": [{"from": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "kube-system"}},
                                      "podSelector": {"matchLabels": {"app.kubernetes.io/name": "traefik"}}}],
                            "ports": [{"port": 8080}, {"port": 8089}]}],
               "egress": [
                   {"to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "kube-system"}},
                            "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}}}],
                    "ports": [{"port": 53, "protocol": "UDP"}, {"port": 53, "protocol": "TCP"}]},
                   {"to": [{"ipBlock": {"cidr": "10.42.3.0/24" if env == "prod" else "10.42.2.0/24"}}],
                    "ports": [{"port": 5432}]},
                   {"to": [{"ipBlock": {"cidr": "0.0.0.0/0", "except": ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "127.0.0.0/8"]}}],
                    "ports": [{"port": 443}]}]}}
    yield {"apiVersion": "v1", "kind": "ServiceAccount", "metadata": {"name": "deploy", "namespace": ns},
           "automountServiceAccountToken": False}
    yield {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
           "metadata": {"name": "deploy", "namespace": ns}, "rules": [
               {"apiGroups": [""], "resources": ["secrets", "configmaps", "services"], "verbs": ["get", "list", "watch", "create", "update", "patch", "delete"]},
               {"apiGroups": [""], "resources": ["pods", "pods/log", "events"], "verbs": ["get", "list", "watch"]},
               {"apiGroups": ["apps"], "resources": ["deployments", "replicasets"], "verbs": ["get", "list", "watch", "create", "update", "patch", "delete"]},
               {"apiGroups": ["batch"], "resources": ["jobs"], "verbs": ["get", "list", "watch", "create", "update", "patch", "delete"]},
               {"apiGroups": ["networking.k8s.io"], "resources": ["ingresses"], "verbs": ["get", "list", "watch", "create", "update", "patch", "delete"]}]}
    yield {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
           "metadata": {"name": "deploy", "namespace": ns},
           "subjects": [{"kind": "ServiceAccount", "name": "deploy", "namespace": ns}],
           "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "deploy"}}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('prefix')
    parser.add_argument('public_ip')
    parser.add_argument('email')
    parser.add_argument('--environment', choices=(*ENVIRONMENTS, 'all'), default='all',
                        help='Environment to configure and rotate credentials for (default: all).')
    args = parser.parse_args(argv)
    prefix, public_ip, email = args.prefix, args.public_ip, args.email
    environments = ENVIRONMENTS if args.environment == 'all' else (args.environment,)
    if not re.fullmatch(r"[a-z][a-z0-9]{3,11}", prefix):
        raise ValueError("invalid prefix")
    token_request = urllib.request.Request(
        "http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=https%3A%2F%2Fvault.azure.net",
        headers={"Metadata": "true"})
    with urllib.request.urlopen(token_request, timeout=30) as response:
        token = json.load(response)["access_token"]

    def secret(env, name):
        request = urllib.request.Request(f"https://{prefix}-{env}.vault.azure.net/secrets/{name}?api-version=7.4",
                                         headers={"Authorization": "Bearer " + token})
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.load(response)["value"]

    # The release URL is pinned, not an installer fetched from main/latest.
    kubectl("apply", "-f", f"https://github.com/cert-manager/cert-manager/releases/download/{CERT_MANAGER}/cert-manager.yaml")
    for deployment in ("cert-manager", "cert-manager-webhook", "cert-manager-cainjector"):
        kubectl("-n", "cert-manager", "rollout", "status", "deployment/" + deployment, "--timeout=300s")
    apply({"apiVersion": "cert-manager.io/v1", "kind": "ClusterIssuer", "metadata": {"name": "letsencrypt"},
           "spec": {"acme": {"email": email, "server": "https://acme-v02.api.letsencrypt.org/directory",
                             "privateKeySecretRef": {"name": "letsencrypt-account"},
                             "solvers": [{"http01": {"ingress": {"ingressClassName": "traefik"}}}]}}})
    kubeconfigs = {}
    for env in environments:
        ns = "fishhawk-" + env
        for document in namespace_documents(env):
            apply(document)
        password = secret(env, "database-password")
        if not re.fullmatch(r"[A-Za-z0-9]{32}", password):
            raise ValueError("unexpected generated database password")
        admin = urllib.parse.urlparse(secret(env, "database-admin-url"))
        pg_env = {**os.environ, "PGHOST": admin.hostname, "PGPORT": "5432", "PGUSER": admin.username,
                  "PGPASSWORD": admin.password, "PGDATABASE": "postgres", "PGSSLMODE": "verify-full",
                  "PGSSLROOTCERT": "/etc/ssl/certs/ca-certificates.crt"}
        role = "fishhawk_" + env
        sql = f"""DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='{role}') THEN CREATE ROLE {role} LOGIN; END IF; END $$;
ALTER ROLE {role} PASSWORD '{password}';
GRANT {role} TO fishhawkadmin;
ALTER DATABASE {role} OWNER TO {role};
REVOKE ALL ON DATABASE {role} FROM PUBLIC;
"""
        subprocess.run(["psql", "-v", "ON_ERROR_STOP=1"], input=sql.encode(), env=pg_env,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True)
        # A failed query must never print SQL containing a password.
        mappings = {"database-url": "FISHHAWKD_DATABASE_URL",
                    "azure-storage-connection-string": "FISHHAWKD_AZURE_STORAGE_CONNECTION_STRING",
                    "github-webhook-secret": "FISHHAWKD_GITHUB_WEBHOOK_SECRET",
                    "github-app-private-key": "github-app-private-key.pem",
                    "oauth-client-secret": "FISHHAWKD_OAUTH_CLIENT_SECRET",
                    "anthropic-api-key": "FISHHAWKD_ANTHROPIC_API_KEY"}
        apply({"apiVersion": "v1", "kind": "Secret", "metadata": {"name": "fishhawk-secrets", "namespace": ns},
               "type": "Opaque", "stringData": {target: secret(env, source) for source, target in mappings.items()}})
        # Explicit credential provisioning: deployment CI never receives cluster-admin.
        # Re-running rotates the token and invalidates its previous version.
        kubectl("-n", ns, "delete", "secret", "deploy-token", "--ignore-not-found")
        apply({"apiVersion": "v1", "kind": "Secret", "metadata": {"name": "deploy-token", "namespace": ns,
               "annotations": {"kubernetes.io/service-account.name": "deploy"}}, "type": "kubernetes.io/service-account-token"})
        kubectl("-n", ns, "wait", "--for=jsonpath={.data.token}", "secret/deploy-token", "--timeout=60s")
        data = json.loads(kubectl("-n", ns, "get", "secret", "deploy-token", "-o", "json"))["data"]
        kubeconfigs[env] = {"apiVersion": "v1", "kind": "Config", "current-context": env,
            "clusters": [{"name": "pilot", "cluster": {"server": f"https://{public_ip}:6443", "certificate-authority-data": data["ca.crt"]}}],
            "users": [{"name": "deploy", "user": {"token": base64.b64decode(data["token"]).decode()}}],
            "contexts": [{"name": env, "context": {"cluster": "pilot", "user": "deploy", "namespace": ns}}]}
    print(json.dumps(kubeconfigs))


if __name__ == "__main__":
    main()
