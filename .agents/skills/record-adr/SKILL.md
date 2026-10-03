---
name: record-adr
description: Add a ratified Fishhawk ADR to the in-repo decision record — transcribe the tracker issue into docs/adr/NNN-slug.md with correct front matter, infer status and applies_to by the documented rules, wire supersession both ways, update the README table, and regenerate index.json. Use when an ADR is accepted/superseded, or when asked to "record", "backfill" or "add" an ADR.
---

# Record an ADR

The format contract is `docs/adr/README.md`; read it before writing. The record **transcribes and never corrects**: the body below the H1 is the tracker issue body byte for byte. The only normalisations are CRLF → LF and exactly one trailing newline.

## 1. Fetch the source

```sh
gh issue view <N> --json number,title,body,url
```

- **Number:** the ADR number comes from the title (`[ADR-NNN] …`).
- **File name:** `docs/adr/NNN-<kebab-slug>.md`, with a zero-padded 3-digit prefix.

## 2. Write the record

```
---
id: ADR-NNN
title: "<issue title without the [ADR-NNN] prefix>"
status: <accepted|proposed|rejected|superseded|unknown>
date: YYYY-MM-DD
issue: https://github.com/kuhlman-labs/fishhawk/issues/<N>
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-NNN: <title>

<issue body, verbatim>
```

### Front-matter grammar

It is restricted:

- one `key: value` per line
- strings are JSON double-quoted, or a bare token
- list keys are JSON arrays, always present (`[]` when empty)
- no unknown or duplicate keys
- `date` is optional

### `status`: fail-closed inference

- **`accepted`:** only when a Decision-headed section states acceptance (`Recorded <date>`, `Accepted`, `Approved`, `Decided`, `Adopt option X`).
- **`unknown`:** a placeholder (`_To be recorded._`) or a design with no acceptance statement. **Never guess upward.** List unknowns for the user to confirm.
- **`superseded` / `rejected`:** only when a source says so.

### `date`

The date the Decision section states. Omit it if none is stated; it is not the implementation date.

### `applies_to`

- **Default:** `[]`, unless the Decision or Consequences section names a repo path as the decision's *subject*. Citations and "see also" don't count.
- **Wildcards:** a `<placeholder>` segment becomes `*`; a directory gets a trailing `**`.
- **Check:** the gate verifies that each glob's literal stem appears in the body as a bounded path token.
- **When in doubt:** `[]`.

### Supersession

Supersession is reciprocal. If the new ADR states it supersedes ADR-X:

- add `ADR-X` to the new record's `supersedes`
- add the new id to ADR-X's `superseded_by`
- set ADR-X's `status: superseded` if the source says so

Both records must exist.

## 3. Index and table

```sh
scripts/check-adr --write-index      # regenerates docs/adr/index.json; never hand-edit it
scripts/check-adr                    # the gate; must pass
```

In `docs/adr/README.md`:

- Add the row under "Records", matching the existing rows:
  ```
  | [ADR-NNN](NNN-slug.md) | <title> | `<status>` | [#N](<issue url>) |
  ```
- If the status is `unknown`, also add a bullet under "Status unknown — needs captain confirmation" that quotes the source verbatim.

## 4. Gate and ship

- **Gate:** `scripts/check-adr` also runs inside `scripts/test verify`.
- **Timing:** commit in the same change that records the ratification, or the one right after. ADR-081 onward are added one at a time under the upkeep rule.
- **Tracker:** if the ADR was just resolved, also edit the issue's Decision section and close it (Git flow step 7).
