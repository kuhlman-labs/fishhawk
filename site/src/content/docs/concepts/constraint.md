---
title: constraint
description: A rule checked against what a stage actually did, not against what it promised.
---

A constraint is a rule Fishhawk checks against the real output of a stage. It is
not advice given to the agent — it is a check run afterwards, against the diff,
by code the agent does not execute.

```yaml
constraints:
  - max_files_changed: 45
  - forbidden_paths: [".github/workflows/**", ".fishhawk/**"]
  - required_outcomes: [tests_added_or_updated, ci_green]
```

- **`max_files_changed`** bounds the blast radius. A change that grew past its
  approved scope fails rather than landing.
- **`forbidden_paths`** are glob patterns the stage may not write. The check is
  on the committed diff, so an agent that edits a forbidden file has failed the
  stage whatever it says in its summary.
- **`required_outcomes`** are conditions the result must satisfy — tests were
  added or updated, CI is green.

## Why the distinction from a gate matters

A [gate](/fishhawk/concepts/gate/) asks a person. A constraint asks the diff. An
agent can write a persuasive explanation of why it needed to touch a forbidden
path; it cannot make the path check return true. When you are deciding how much
autonomy to grant, the question worth asking is which of your rules are gates —
persuadable — and which are constraints.

## Scope

The plan's declared file scope acts as a constraint too. A file the agent edits
without declaring is dropped from the commit; a file it creates without
declaring fails the stage. Widening the scope mid-stage is a request that goes
back to the operator, not a decision the agent makes.

## Related

- [plan](/fishhawk/concepts/plan/) — where the declared scope comes from.
