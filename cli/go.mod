module github.com/kuhlman-labs/fishhawk/cli

go 1.25

require (
	github.com/bmatcuk/doublestar/v4 v4.10.0
	github.com/google/uuid v1.6.0
	github.com/kuhlman-labs/fishhawk/credstore v0.0.0
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3
	gopkg.in/yaml.v3 v3.0.1
)

require golang.org/x/text v0.14.0 // indirect

// credstore ships in this repo and has no upstream tag yet, so it resolves
// from the filesystem rather than a pseudo-version. A filesystem-replaced
// module records no go.sum entries by design.
replace github.com/kuhlman-labs/fishhawk/credstore => ../credstore
