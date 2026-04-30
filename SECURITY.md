# Security Policy

## Reporting a vulnerability

Please do not file public GitHub issues for security vulnerabilities.

Report security issues by email to **brett.kuhlman@proton.me** with a subject line beginning with `[fishhawk-security]`. If you would like to encrypt the report, request a PGP key in your initial message and one will be provided.

A report should include:

- A description of the issue and its impact
- Steps to reproduce (or a proof-of-concept) if possible
- The version, commit, or environment where the issue was observed
- Whether you intend to disclose the issue publicly, and on what timeline

## What to expect

- Acknowledgement of the report within 3 business days.
- An initial assessment within 7 business days.
- Coordinated disclosure: a fix and a public advisory will be prepared together. We will agree on a disclosure date with the reporter; the default is 90 days from acknowledgement, sooner if a fix is ready.
- Credit in the public advisory if the reporter wishes.

## Scope

This policy covers code in this repository and any artifacts published from it (the `fishhawk` CLI, the `fishhawk/runner` GitHub Action, the backend control plane once it ships, and the GitHub App once it is registered).

Out of scope: third-party services Fishhawk integrates with (GitHub, Anthropic, etc.) — please report those to the relevant vendor.

## Supported versions

Until Fishhawk reaches v0.1, only the latest commit on `main` is supported.
