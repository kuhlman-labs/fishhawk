module github.com/kuhlman-labs/fishhawk/cli

go 1.25

require (
	github.com/bmatcuk/doublestar/v4 v4.10.2
	github.com/google/uuid v1.6.0
	github.com/kuhlman-labs/fishhawk/credstore v0.0.0-20261008230330-b19bca56601f
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3
	gopkg.in/yaml.v3 v3.0.1
)

require golang.org/x/text v0.14.0 // indirect

// credstore ships in this repo and is pinned by PSEUDO-VERSION (no replace
// directive) so `go install github.com/kuhlman-labs/fishhawk/cli/cmd/fishhawk@…`
// works: go install refuses a module whose go.mod carries a replace (#4117).
// Locally, go.work's `use ./credstore` still wins over this pin. After a
// credstore change the CLI consumes merges, bump the pin per cli/README.md
// § "Install"; cmd/fishhawk/gomod_test.go pins the no-replace precondition.
