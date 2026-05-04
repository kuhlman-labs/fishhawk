#!/usr/bin/env bash
# Render docs/github-app/manifest.template.json with the backend
# URL filled in. Used by the operator before submitting to
# GitHub's manifest-flow endpoint (E4.1 / #48).
#
# Usage:
#   scripts/render-github-app-manifest.sh https://api.fishhawk.example.com
#
# The rendered JSON is printed to stdout; redirect or pipe as
# needed:
#
#   scripts/render-github-app-manifest.sh https://api.fishhawk.example.com \
#     | jq .   # validate
#
#   scripts/render-github-app-manifest.sh https://api.fishhawk.example.com \
#     | pbcopy # macOS — paste into the form
#
# The manifest URL itself doesn't include the backend URL;
# GitHub resolves placeholders from the JSON we POST to it.

set -euo pipefail

if [[ $# -ne 1 ]]; then
    echo "usage: $0 <backend-url>" >&2
    echo "  e.g. $0 https://api.fishhawk.example.com" >&2
    exit 2
fi

backend_url="$1"

# Strip trailing slash so the rendered URLs don't double up on /.
backend_url="${backend_url%/}"

if [[ -z "$backend_url" || "$backend_url" != http* ]]; then
    echo "$0: backend URL must start with http:// or https://" >&2
    exit 2
fi

template="$(dirname "$0")/../docs/github-app/manifest.template.json"
if [[ ! -f "$template" ]]; then
    echo "$0: template not found: $template" >&2
    exit 1
fi

# Use sed with a delimiter that won't appear in URLs.
sed "s|{{BACKEND_URL}}|${backend_url}|g" "$template"
