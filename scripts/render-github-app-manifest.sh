#!/usr/bin/env bash
# Render docs/github-app/manifest.template.json with the backend
# URL and webhook URL filled in. Used by the operator before
# submitting to GitHub's manifest-flow endpoint (E4.1 / #48).
#
# Usage:
#   scripts/render-github-app-manifest.sh <backend-url> <webhook-url>
#
# Examples:
#   # Production: webhook hits the backend directly.
#   scripts/render-github-app-manifest.sh \
#     https://api.fishhawk.example.com \
#     https://api.fishhawk.example.com/webhooks/github
#
#   # Local dev: backend on localhost, webhooks via smee.io.
#   scripts/render-github-app-manifest.sh \
#     http://localhost:8080 \
#     https://smee.io/abc123
#
# The webhook URL is required because GitHub's manifest validator
# rejects manifests with a blank `hook_attributes.url`. For local
# dev where the backend isn't internet-reachable, point it at a
# smee.io forwarder.
#
# The rendered JSON is printed to stdout; redirect or pipe as
# needed:
#
#   scripts/render-github-app-manifest.sh ... | jq .   # validate
#   scripts/render-github-app-manifest.sh ... | pbcopy # macOS

set -euo pipefail

if [[ $# -ne 2 ]]; then
    echo "usage: $0 <backend-url> <webhook-url>" >&2
    echo "  e.g. $0 https://api.fishhawk.example.com https://api.fishhawk.example.com/webhooks/github" >&2
    echo "  e.g. $0 http://localhost:8080 https://smee.io/abc123" >&2
    exit 2
fi

backend_url="$1"
webhook_url="$2"

# Strip trailing slash so the rendered URLs don't double up on /.
backend_url="${backend_url%/}"
webhook_url="${webhook_url%/}"

if [[ -z "$backend_url" || "$backend_url" != http* ]]; then
    echo "$0: backend URL must start with http:// or https://" >&2
    exit 2
fi

if [[ -z "$webhook_url" || "$webhook_url" != http* ]]; then
    echo "$0: webhook URL must start with http:// or https://" >&2
    exit 2
fi

template="$(dirname "$0")/../docs/github-app/manifest.template.json"
if [[ ! -f "$template" ]]; then
    echo "$0: template not found: $template" >&2
    exit 1
fi

# Use sed with a delimiter that won't appear in URLs.
sed -e "s|{{BACKEND_URL}}|${backend_url}|g" \
    -e "s|{{WEBHOOK_URL}}|${webhook_url}|g" \
    "$template"

# The App manifest schema has no device-flow key (GitHub's manifest
# parameter set is fixed), so this step can't be automated. Printed to
# stderr, after stdout, so `| jq .` / `| pbcopy` pipelines see only the
# rendered JSON.
cat >&2 <<'EOF'

Post-creation checklist:
  [ ] Enable Device Flow — GitHub → Settings → Developer settings →
      GitHub Apps → the app → General → check 'Enable Device Flow' →
      Update application.
      Until this is checked, every `fishhawk token login` fails with
      device_flow_disabled.
EOF
