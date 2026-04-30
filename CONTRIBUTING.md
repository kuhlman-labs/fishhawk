# Contributing to Fishhawk

Thanks for your interest. A few things to know before you open an issue or a pull request.

## Project status

Fishhawk is in pre-alpha. The v0 abstractions described in [`docs/MVP_SPEC.md`](docs/MVP_SPEC.md) are still being fixed in place. Until those are stable, the project is not accepting feature PRs from outside contributors. Bug reports, design discussion, and feedback on the spec are welcome and useful.

## Issues

- **Bugs:** use the bug report template. Include the smallest reproduction you can.
- **Feature requests / design feedback:** use the feature request template. Reference the section of [`docs/MVP_SPEC.md`](docs/MVP_SPEC.md) you are responding to.
- **Security issues:** do not file a public issue. See [`SECURITY.md`](SECURITY.md).

## Pull requests (when we begin accepting them)

- One logical change per PR. Small is better than thorough.
- Reference the issue the PR addresses.
- Tests are required for behavior changes.
- Once Fishhawk is self-hosting (target: day 21 of the v0 build), every PR will flow through a Fishhawk workflow run, including PRs from external contributors. The workflow spec at [`.fishhawk/workflows.yaml`](.fishhawk/workflows.yaml) is the source of truth for what that means in practice.

## Developer Certificate of Origin (DCO)

Fishhawk uses the [Developer Certificate of Origin](https://developercertificate.org/) rather than a CLA. By signing off on your commits, you are certifying that you wrote the patch or otherwise have the right to submit it under the project's license.

Sign off your commits with `git commit -s`. The trailer looks like:

```
Signed-off-by: Your Name <you@example.com>
```

PRs without sign-off will be asked to amend.

## Code of Conduct

Participation in this project is governed by the [Contributor Covenant](CODE_OF_CONDUCT.md). Report concerns to brett.kuhlman@proton.me.

## License

By contributing, you agree that your contributions will be licensed under the Apache License, Version 2.0. See [`LICENSE`](LICENSE).
