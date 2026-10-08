# backend/internal/alerttrigger

Alert ingress for ADR-053 option A (E35.4 / #1601). `POST /v0/triggers/alert`
accepts an HMAC-signed alert and files a conventions-complete incident issue
through the work-items pipeline. Alerts are deduplicated by fingerprint: a
repeat comments on the existing issue instead of filing a new one. A source
can optionally auto-start a hotfix run on the filed issue. That option is per
source and ships **OFF**.

| File | Owns |
|---|---|
| `verify.go` | Signing string, header parsing, constant-time MAC check, replay window. |
| `config.go` | The sources file (`FISHHAWKD_ALERT_SOURCES_FILE`): strict YAML, `secret_env` indirection, defaults. |
| `payload.go` | Strict alert JSON, incident-issue sections, occurrence comment, untrusted-text rendering. |
| `store.go` | The `alert_incidents` dedup ledger (migration 0099): claim / complete / release / record-run. |

The HTTP handler (`backend/internal/server/alert_trigger.go`), the in-process
auto-start (`server.StartAlertRun`) and the fishhawkd flags live outside this
package.

## Wire contract

```
POST /v0/triggers/alert
Content-Type: application/json
X-Fishhawk-Alert-Source:    <source id>
X-Fishhawk-Alert-Timestamp: <Unix seconds, 1-12 ASCII digits>
X-Fishhawk-Alert-Signature: sha256=<hex HMAC-SHA256(secret, timestamp + "." + raw body)>
```

- **Signing string**: the timestamp header value exactly as sent, one `.`, then
  the raw request body bytes. Because the timestamp is signed, it cannot be
  changed after signing. `Sign(secret, timestamp, body)` produces the header
  value. The hex digest is case-insensitive.
- **Bearer auth does not apply.** The route is authenticated only by the
  per-source HMAC, not by an API token. It is a new route, and no existing
  token's access changes.
- **Verification order** (`Verify`), each failure a distinct error:
  1. The signature header is absent, has no `sha256=` prefix, has an empty
     digest, or is not hex: `ErrSignatureMissing`.
  2. The timestamp is not 1-12 ASCII digits (no sign, whitespace or fraction):
     `ErrTimestampInvalid`.
  3. The MAC is compared with `crypto/hmac.Equal`, the
     `backend/internal/webhook.VerifySignature` precedent. A mismatch returns
     `ErrSignatureInvalid`. An **unknown source** also returns
     `ErrSignatureInvalid`, after computing and comparing a MAC under a fixed
     dummy key, so it does the same work and gives the same answer as a
     mis-signed request. A caller cannot enumerate source ids.
  4. Only a valid MAC reaches the window check: a timestamp more than
     `window` before or after `now` returns `ErrStale`. An unsigned or
     mis-signed stale request reports the signature failure. The comparison
     is between instants (`now.Before(t.Add(-window)) ||
     now.After(t.Add(window))`), never a signed `time.Duration`: for a
     validly signed far-future timestamp such as `999999999999` (~31,700
     years ahead) `now.Sub(t)` saturates to the minimum Duration and its
     negation wraps to itself, so an `|now - t| > window` test would accept
     the request. Pinned by `TestVerify_FarFutureTimestampIsStale` (with a
     far-past row, which does not overflow, kept separately).
- **Replay window**: default 5m (`DefaultReplayWindow`), set by
  `FISHHAWKD_ALERT_REPLAY_WINDOW` in `(0, 15m]`. It applies in both directions,
  so a sender whose clock is skewed past the window is refused.
- **Nonce**: the window alone admits an exact resend inside the window. After
  `Verify` succeeds, the handler marks
  `"alert:" + source + ":" + hex(decoded MAC)` in the webhook delivery store
  (Postgres-backed, 24h retention). The key is the **decoded** MAC that
  `Verify` returns, so a resend with the hex case flipped is still a
  duplicate. Unsigned and mis-signed requests never reach `Mark`, so an
  unauthenticated caller cannot fill the store.

Sender sketch (POSIX shell + openssl):

```sh
ts=$(date +%s)
sig=$(printf '%s.%s' "$ts" "$body" | openssl dgst -sha256 -hmac "$ALERT_SECRET" -hex | sed 's/^.* //')
curl -sS -X POST "$FISHHAWK_URL/v0/triggers/alert" \
  -H 'Content-Type: application/json' \
  -H "X-Fishhawk-Alert-Source: grafana-prod" \
  -H "X-Fishhawk-Alert-Timestamp: $ts" \
  -H "X-Fishhawk-Alert-Signature: sha256=$sig" \
  --data-binary "$body"
```

## Sources file

`FISHHAWKD_ALERT_SOURCES_FILE` names a YAML file. Unset means the ingress is
off and the route answers 503. fishhawkd **fails startup** on a malformed
file, an unknown key at any level, a version other than 1, an empty `sources`
list, more than one YAML document, or any per-source refusal below.

```yaml
version: 1
sources:
  - id: grafana-prod            # required; ^[a-z0-9][a-z0-9_-]{0,63}$, unique
    secret_env: GRAFANA_ALERT_SECRET  # required; env var holding the secret (>= 32 bytes)
    repo: acme/shop             # required; owner/name the incident is filed in
    work_item_type: bug         # default bug; a conventions type
    parent_epic: "#35"          # optional; #N or N
    labels: [area:backend, phase:beta]  # optional; merged onto the type's defaults
    auto_start: false           # default false; see "Auto-start" below
    workflow_id: hotfix_change  # default hotfix_change
    runner_kind: local          # optional; github_actions | local | gitlab_ci
```

- Secrets never live in the file. `secret_env` names the environment variable
  that carries the secret. An unset variable, or a value shorter than 32 bytes
  (`MinSecretBytes`, the HMAC-SHA256 output size), refuses the source. No
  refusal echoes the secret. `Source` formats with the secret redacted under
  every `fmt` verb, and `Sources` formats as its configured ids only
  (`alerttrigger.Sources{ids:[a b]}`). The second method is load-bearing: fmt
  does not invoke formatting methods on unexported fields, so without it
  `%+v` on a `Sources` reflects into the unexported map and prints each raw
  secret byte slice. Pinned by `TestSecretNeverRendered`.
- A YAML syntax or type error is reported with the decoder's own text
  wrapped (`alert sources: yaml: unmarshal errors: ... cannot unmarshal ...`),
  so the startup failure says why the file was refused.
- `auto_start` accepts only a YAML boolean (`true` / `false`). yaml.v3 would
  otherwise coerce YAML 1.1 spellings (`yes`, `on`, `"yes"`) into a bool, and a
  typo must not arm auto-start.
- Under this repo's work-management defaults, the `bug` title is
  `[E{epic}.{n}] {summary}`, so a source needs `parent_epic` for filing to
  succeed. A source without it fails closed on the first alert. The claim is
  released and the delivery unmarked, so the same alert can be retried after
  the config is fixed.

## Alert payload

One JSON object (`ParseAlert`). Unknown fields and trailing data are refused,
and every refusal wraps `ErrInvalidAlert` (400 `validation_failed`).

| Field | Rule |
|---|---|
| `fingerprint` | Required. 1-200 chars of `[A-Za-z0-9._:/-]`. The dedup key. |
| `title` | Required. 1-200 characters, not blank, no control characters (see below). |
| `severity` | Required. `critical` \| `high` \| `medium` \| `low` \| `info`. |
| `description` | Optional. At most 16 KiB. |
| `url` | Optional. Absolute `http`/`https` with a host, at most 2048 characters. No whitespace, no control characters, and none of `<`, `>`, `"`, `\`, or a backtick. |
| `environment` | Optional. At most 64 of `[a-z0-9_-]`. |
| `labels` | Optional. At most 32 entries; key 1-64 and value at most 256 characters, no control characters (see below). |

"Control character" in the single-line fields (`title`, label keys and
values, `url`) means any of: a C0/C1 control (`unicode.IsControl`, category
Cc); U+2028 LINE SEPARATOR or U+2029 PARAGRAPH SEPARATOR (Zl/Zp, a line break
to many renderers); or a bidirectional formatting character, U+202A-U+202E
(embeddings and overrides) or U+2066-U+2069 (isolates), category Cf per
UAX #9, which can make rendered text read differently from its bytes.
`unicode.IsControl` is false for all eleven of the latter, so `hasControl`
lists them explicitly. Pinned by `TestParseAlert_RefusesLineAndBidiControls`
(one row per rune).

The issue summary is `Incident (<severity>): <title>`. `IncidentSections`
fills the bug skeleton sections Summary, Observed (source, severity,
environment, URL, fingerprint, labels, received time, then the description),
Proposal, Done-means, Acceptance criteria and Notes. When `auto_start` is off,
Notes carries the next step: start a `<workflow_id>` run on the issue. When it
is on, Notes says a run was requested automatically and that its outcome is on
the `alert_incident_filed` audit entry. `OccurrenceComment` renders the comment
a repeat alert posts.

### Untrusted text

Alert text reaches a forge issue and, through it, later agent prompts. The
UNTRUSTED ISSUE prompt envelope applies downstream. In addition, the renderer:

- Neutralizes every `<!--` (a zero-width space after `<!`). A sender cannot
  forge a Fishhawk hidden marker: the idempotency key (a whole-line match), or
  the `fishhawk-fingerprint` / `fishhawk-sticky` / upkeep markers (substring
  matches).
- Puts the description in a fenced block longer than any backtick run it
  contains, with every line blockquoted (`> `). No sender line can close the
  fence or begin a raw body line, which keeps the line-anchored `Parent epic:`
  and `Depends on:` markers unforgeable.
- Renders single-line text (title, labels, environment) as code spans sized the
  same way, so links and `@mentions` stay inert.

Pinned by `TestIncidentSections_NeutralizesMarkers`,
`TestIncidentSections_DescriptionCannotStartBodyLine` and
`TestIncidentSections_FenceNotBroken`.

## Dedup

The key is `(source id, repo, fingerprint)` in `alert_incidents`. `Claim`
answers one of:

- `new`: the caller files, then calls `Complete`, or `Release` on a filing
  failure.
- `existing`: the caller comments on the filed issue.
- `in_flight`: another caller holds a live claim. Nothing is filed or
  commented, and the occurrence is counted.

A claim whose filer has not completed within 10 minutes is reclaimed with a
new token. The cutoff is measured on the DB clock. The claim token stops a late
original filer from overwriting the new claimant (`ErrClaimLost`, surfaced as
`dedup_claim_lost: true`).

Dedup lives in Postgres, not forge search, because a search index is
eventually consistent and a fast re-fire could double-file.

Stated limits:

- If a filing succeeds on the forge but its `Complete` is lost to a stale
  reclaim, a second issue can be filed. The hidden idempotency marker
  (`alert-incident` namespace) names the duplicate.
- A re-fire after the incident issue is **closed** still comments on that
  closed issue. The ledger does not read forge state.
- The same fingerprint from two sources, or into two repos, is two incidents.
- An `in_flight` answer releases the nonce (see "Mark/Unmark contract"), so a
  sender's exact retry of that request is counted again in `occurrences` (and,
  once the issue is filed, posts an occurrence comment). `occurrences` counts
  accepted deliveries, not distinct alert firings.
- There is no tenant scoping. The ingress authenticates a SOURCE, and
  `alert_incidents` is deployment-level metadata like `webhook_deliveries`.
  Multi-tenant source scoping is an E44 follow-up.

## Auto-start (default OFF)

`auto_start` is **false unless the file sets `auto_start: true`**. That is the
ADR-053 fork-2 safety property: auto-starting a hotfix from an external signal
is an operator configuration decision. fishhawkd logs a WARN at startup naming
every source with `auto_start: true` (`Sources.AutoStartIDs`).

When it is on, a newly filed incident starts a `workflow_id` run in process
(`server.StartAlertRun`) with `trigger_source: alert` and `trigger_ref:
issue:<N>`. `alert` is a SYSTEM-ONLY trigger source: `POST /v0/runs` refuses it
from every caller with 400 `trigger_source_reserved`. Every admission gate
still applies, and the run stops at every gate. An auto-start failure is
reported in the response's `auto_start` object and on the audit row. It never
fails the filing.

**Trigger-form contract:** an `alert` run maps to `spec.TriggerDiff`
(`appliesto.TriggerFormForSource`). A workflow an alert source auto-starts,
including the E35.5 `hotfix_change` preset (#1602), must list `diff` in its
`applies_to.trigger`. Otherwise every auto-started alert run is refused 422
`workflow_not_applicable`. Until that workflow exists in the target repo, an
auto-start is refused, and the incident issue is still filed. The preset ships
as `docs/spec/examples/workflow-v2-hotfix-change.yaml` (reference:
`docs/spec/workflow-v2.md` § "Incident hotfix preset (hotfix_change)"); an
operator copies its stanza into the target repo's `.fishhawk/workflows.yaml`.
`server.TestStartAlertRun_ShippedHotfixChangePresetAdmitted` drives the shipped
bytes through `StartAlertRun` and pins the contract, with a `[scheduled]`
variant that is refused 422.

**Known limitation (ADR-053 forks 2 + 3):** a production-originated alert can
auto-start a hotfix whose post-deploy verification targets staging only. The
`hotfix_change` run itself carries no post-deploy acceptance stage: a hotfix
gets post-deploy verification only when the operator starts the follow-on
`release` run (`docs/spec/examples/workflow-v2-release-acceptance.yaml`).

## Response codes

### Mark/Unmark contract

The nonce is marked (`Mark`) only after `Verify` succeeds, so an
unauthenticated caller never writes to the delivery store. After the mark,
**only two outcomes keep the nonce**: 201 `filed` (an issue was durably filed)
and 200 `occurrence` (a comment was durably posted). Every other post-Mark
branch calls `Unmark` before answering, mirroring `webhook_gitlab.go`: the 400
payload refusal, a conventions-load failure, a `resolveRepoScope` failure, 501
`provider_unimplemented`, a `Claim` error, a filing error (after `Release` of
the claim token), an occurrence-comment failure (502
`alert_occurrence_failed`), and the 202 `in_flight` answer. An exact resend of
a request that took one of those branches is therefore processed again instead
of being refused `alert_replayed`, so a sender's retry lands once the
transient fault or the competing claim resolves. That is safe because filing
is deduplicated by the `alert_incidents` claim and the hidden idempotency
marker, not by the nonce. An auto-start failure after a successful filing
does not change the 201, so the nonce is kept. The handler's per-branch
transient-retry test is `TestAlertTrigger_PostMarkFailureUnmarks`
(`backend/internal/server/alert_trigger_test.go`).

| Status | Code / action | When | Nonce |
|---|---|---|---|
| 503 | `alert_trigger_unconfigured` | No sources configured. | not marked |
| 503 | `alert_store_unconfigured` | Incident store, delivery store or GitHub client missing. | not marked |
| 413 | `body_too_large` | Body over 64 KiB. | not marked |
| 401 | `alert_signature_missing` | `ErrSignatureMissing`. | not marked |
| 401 | `alert_timestamp_invalid` | `ErrTimestampInvalid`. | not marked |
| 401 | `alert_signature_invalid` | `ErrSignatureInvalid` (mis-signed or unknown source). | not marked |
| 401 | `alert_replayed` (`reason: stale`) | `ErrStale`. | not marked |
| 401 | `alert_replayed` (`reason: duplicate`) | `Mark` found the decoded MAC already recorded. | already marked |
| 400 | `validation_failed` | `ParseAlert` refused the body. | **Unmark** |
| 501 | `provider_unimplemented` | Conventions provider is not `github_projects`. | **Unmark** |
| 5xx | conventions / repo-scope error | Conventions load or `resolveRepoScope` failed. | **Unmark** |
| 5xx | claim error | `Claim` failed. | **Unmark** |
| work-item status | work-item error code | Filing failed. The claim is `Release`d. | **Unmark** |
| 502 | `alert_occurrence_failed` | Occurrence comment failed. | **Unmark** |
| 201 | `action: filed` | Issue filed (`auto_start` outcome included, `dedup_claim_lost` when `Complete` lost the claim). | kept |
| 200 | `action: occurrence` | Comment posted on the existing issue. | kept |
| 202 | `action: in_flight` | Another filer holds the claim. The occurrence is counted; nothing is filed or commented. | **Unmark** |

Rejections are not audited, so an unauthenticated caller cannot append to the
audit chain. They are WARN-logged with the source id and code, never the body.
Accepted alerts write the global-chain audit rows `alert_incident_filed` and
`alert_incident_occurrence`.
