---
title: Versioning
description: This site is unversioned and tracks main; spec majors are carried as content, not as site versions.
---

Two different things get called "versioning" here, and they are decided
separately.

## The site is unversioned

This site tracks `main`. There are no versioned documentation snapshots, and no
version picker. Every deploy replaces the previous one.

**The cost, stated plainly:** if you are running an older build of Fishhawk, this
site describes something ahead of what you have. A page may document a flag your
binary does not accept. What to do about it: check your build's commit — `GET
/healthz` reports `git_sha`, as does `fishhawk version` — and read the
repository at that commit for anything that looks newer than your deployment.
The [`docs/`](https://github.com/kuhlman-labs/fishhawk/tree/main/docs) tree is
versioned by git, so it is always readable at the exact commit you are running.

**Revisit trigger:** when Fishhawk ships supported numbered releases, revisit
this and adopt per-release snapshots. Until there are releases to snapshot,
versioned docs would be versioning `main` against itself.

## Spec majors are content, not site versions

Three majors of the workflow spec — `workflow-v0`, `workflow-v1`,
`workflow-v2` — are live simultaneously. That is not three release lines: a
single `main` embeds, routes and validates all three, and a document reaches one
of them by its own `version:` field.

So they are carried as **content**. The [support
table](/fishhawk/reference/#workflow-spec-major-support) on the reference
landing page gives each major's status, and there is **one** [workflow spec
page](/fishhawk/reference/workflow-spec/) covering all three, with the
differences called out inline.

### Why one page, and not one page per major

Three co-equal per-major pages were considered and rejected, for two reasons.
First, the canonical agent-consumed reference already moved away from that
shape deliberately: `docs/spec/workflow-v2.md` was made a complete standalone
reference for the live major, and `workflow-v0.md` and `workflow-v1.md` were
demoted to frozen majors you read to understand an existing document rather than
to look up a current field. Mirroring an abandoned structure on the human-facing
site would put the two surfaces at odds. Second, presenting three co-equal pages
signals that authoring a new v0 spec is a live choice, when the product's own
posture discourages it — `fishhawk init` emits v2 presets, and the reuse,
shape and autonomy grammar exists only at v2.

**Revisit trigger:** if a future major ever diverges far enough that calling the
differences out inline stops being readable, split the spec page per major
*then* — not pre-emptively, and not because a new major exists.

### What is not being claimed

Frozen does not mean deprecated-with-a-date. v0 and v1 stay validated forever,
a new `0.x` or `1.x` document is still accepted, and no removal is planned. The
support table says exactly that, and it is the page to update if that posture
ever changes.
