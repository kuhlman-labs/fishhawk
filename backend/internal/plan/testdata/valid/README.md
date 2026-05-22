# testdata/valid

`example.json` is a human-readable canonical example kept for documentation and
as a stable reference for tests that verify the documented shape (e.g.
`TestParse_Example`).

## Keeping fixtures in sync

When a required schema field is added to `standard_v1`, update
`planfixture.Valid()` in `../planfixture/planfixture.go` first — the schema
compliance test `TestValid_SchemaCompliant` catches missing required fields.
Then update `example.json` here to include the new field; the parity test
`TestValid_ParityWithExample` will fail until example.json is updated.
