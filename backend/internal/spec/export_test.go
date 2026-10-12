package spec

import "github.com/santhosh-tekuri/jsonschema/v6"

// SchemaErrorLeafKind exposes the unexported keyword kind schemaErrorFrom
// records on a SchemaError, so the external spec_test package can assert
// the exact rejecting keyword (#4311 / f8ae15a1).
func SchemaErrorLeafKind(e *SchemaError) jsonschema.ErrorKind { return e.leafKind }
