# Case: seed-synthetic-restates-approach-observable

**Provenance: SYNTHETIC.** This case is a hand-authored plan-review miss
(E55.23 / #4361), NOT a captured production triage entry. It plants exactly
one defect the standard plan-review criteria should catch — a criterion that
restates an approach step while reading like an observable HTTP outcome — so
the plan-review catch-rate gate (`planreviewcatch.go`) has a committed,
discriminating fixture. Nothing here is a real run, issue or repository: the
`example-org/uploads-service` issue, the run ids and the triage sequence are
fabricated placeholders, and `miss.json` carries `"synthetic": true`.

## What it represents

The issue requires every upload to be scanned for malware before download: an
infected file is never served (`GET /files/{id}` returns 410 `file_infected`)
and a pending one returns 409 `scan_pending`. Approach step 2 routes an upload
through a new component — the handler enqueues a scan job on `ScanQueue` and
returns 202. The plan's explicit criterion `ac-upload-enqueues-scan` wraps that
implementation fact in an HTTP observation: "POST /uploads returns HTTP 202
after the handler enqueues a scan job on ScanQueue". It restates step 2 and is
not what the issue asks to observe. The issue's central guarantee has its own
correct criterion, `ac-download-infected-410` (an infected file returns 410
`file_infected`), so the restatement is the plan's only criterion defect. The implementation scanned through a different mechanism,
acceptance failed the criterion, and triage classified it class-3.

Mid-difficulty by design: unlike `seed-synthetic-restates-approach` ("the code
in approach step 2 was written"), the criterion names a route and a status
code, so it reads like an observable outcome.

## The planted defect

- Criterion id: `ac-upload-enqueues-scan` (present, byte-identical, in both
  `miss.json` and the plan inside `review_input.json` — the loader enforces
  both).
- Plan-review criterion it violates: 11 (Independence — criteria are not
  restatements of the approach steps). Because the criterion is labeled
  `explicit` against an issue that never mentions a queue or a 202, a
  reviewer can also correctly flag it under criterion 8a (issue fidelity);
  the catch probes cover that route too, so such a catch scores CAUGHT, not
  as a `no_probe` scoring miss.

## Review input

`review_input.json` carries the issue text the reviewer sees, the complete
`standard_v1` plan under review, the `catch_probes` (case-folded substrings
matched against each emitted concern's note and category; they include the
issue-fidelity phrasing beside the restatement phrasing), the
`attribution_phrases` (substrings of the planted criterion's statement and of
no other criterion's), the `defect_phrases` the prompt-leak self-check reads,
and the hand-written `catching_examples` / `non_catching_examples` the loader
uses to prove the rule discriminates.

## Calibration rework (Step 1c dry run, fix-up concern 4d935789)

Miss class: mostly `not_mentioned` on the planted defect (1/7 caught). The
original plan had an UNINTENDED, louder defect: no criterion covered the
issue's central guarantee (an infected file makes `GET /files/{id}` return
410 `file_infected`). 7/7 reviewers led with that gap and missed the planted
restatement. The plan now carries the blocking explicit criterion
`ac-download-infected-410`, and the test strategy names the infected-file
handler test, so the planted restatement is the only criterion defect. The
catching example that said "no criterion checks 410" and the non-catching
example asking for an infected-file test were rewritten to match the new plan.

The probes were also widened from real catches that matched no probe
(`no_probe`): `approach restatement`, `partly an approach restatement`, `never
states`, `never specifies`, `plan-chosen design value`. None occurs in the
issue or plan (mode m) or in a non_catching_example (mode j). One catching
example phrased that way was added; it scores CAUGHT only through the added
probes.
