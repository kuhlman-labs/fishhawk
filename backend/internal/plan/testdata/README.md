# testdata

`valid/` holds the plan-artifact canonical example (see `valid/README.md`).

## govulncheck fixtures (#3750)

`govulncheck-symbol.json` and `govulncheck-package.json` are **captured**
`govulncheck -json` message streams, not hand-authored. Every message in them
is a byte-for-byte span of the real output; the only edit is the filter below,
which drops whole messages.

`upkeepadvisory_test.go` feeds them through the upkeep-report advisory rules:
each `finding` trace is decoded into `plan.UpkeepAdvisoryFrame` with
`DisallowUnknownFields` and also spliced, as RAW JSON, into an upkeep report
validated against the schema (`advisory-frame` has `additionalProperties:
false`), so a govulncheck frame key the schema does not know is caught rather
than silently dropped.

### Capture

| | |
|---|---|
| Captured | 2026-10-05 |
| Scanner | `govulncheck@v1.7.0` (`GOWORK=off go install golang.org/x/vuln/cmd/govulncheck@v1.7.0`; v1.8.0 needs go >= 1.26) |
| Go | `go1.25.6` darwin/arm64 (the repository pin) |
| DB | `https://vuln.go.dev`, last modified `2026-10-01T20:24:15Z` (recorded in each stream's `config` message) |
| Command | `GOWORK=off govulncheck -json ./...`, run in the fixture module's directory |

Fixture modules (one `main.go` each, `go.mod` requiring
`golang.org/x/text v0.3.7`, `go 1.25.0`):

- `example.com/upkeepfixture/symbol` CALLS the vulnerable symbol:

  ```go
  tags, _, err := language.ParseAcceptLanguage("en-US,en;q=0.9")
  ```

- `example.com/upkeepfixture/package` only IMPORTS the vulnerable package:

  ```go
  fmt.Println(language.English)
  ```

Raw output SHA-256 before filtering: symbol
`0350bdf29229719c320e535fe0ff77066675bb777b3ff539c7f4565a181480ed`, package
`ada06de47987c4a2f022d0f9a4c660ca2404e00f48dce806c43021b7a5524e24`.

### Filter

The raw streams are ~400 KB each, almost all `osv` and `finding` messages for
the Go 1.25.6 standard library. The filter keeps the `config`, `SBOM` and
`progress` messages, every `finding` whose `trace[0].module` is
`golang.org/x/text`, and the `osv` messages those findings name
(`GO-2022-1059`, `GO-2026-5970`); it drops every other message whole. In
Python: decode the stream with `json.JSONDecoder.raw_decode`, keep each
retained message's original source span, join the spans with `\n`.

What the filtered streams carry (the levels the test derives):

| Fixture | `GO-2022-1059` deepest finding | `GO-2026-5970` |
|---|---|---|
| symbol | symbol level: `trace[0]` names `ParseAcceptLanguage`, `trace[1]` the fixture's `main` (vulnerable end first) | module level only |
| package | package level: `trace[0]` names `golang.org/x/text/language`, no function | module level only |

### Re-capturing

Re-run the command above against the same two modules and re-apply the filter.
A newer DB may add `golang.org/x/text` advisories; the test keys on
`GO-2022-1059`, so extra advisories do not break it. The live operator walk
of the upkeep scan remains the end-to-end check; these fixtures pin the frame
shape and trace ordering the server's rules rely on.
