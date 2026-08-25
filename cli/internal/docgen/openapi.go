package docgen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// openapiRelPath is the canonical OpenAPI document.
const openapiRelPath = "docs/api/v0.openapi.yaml"

// httpMethods are the operation-bearing keys under a path item; anything
// else (parameters, summary, description, servers) is not an operation.
var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

func isHTTPMethod(k string) bool {
	for _, m := range httpMethods {
		if k == m {
			return true
		}
	}
	return false
}

// OperationKey is one (METHOD, path) pair — the unit of API coverage.
type OperationKey struct {
	Method string // upper-case
	Path   string
}

// RowToken is the byte-stable per-operation prefix a rendered row carries
// (the code-spanned method and path cells), used by the coverage test to
// assert each operation appears exactly once.
func (k OperationKey) RowToken() string {
	return codeSpan(k.Method) + " | " + codeSpan(k.Path)
}

// LoadOperations returns every operation in the OpenAPI document, in
// document order, as the coverage authority.
func LoadOperations(root string) ([]OperationKey, error) {
	data, err := os.ReadFile(filepath.Join(root, openapiRelPath))
	if err != nil {
		return nil, err
	}
	rootNode, err := rootMapping(data)
	if err != nil {
		return nil, err
	}
	paths := mapVal(rootNode, "paths")
	if paths == nil {
		return nil, fmt.Errorf("docgen: openapi document has no paths")
	}
	var ops []OperationKey
	for _, p := range mapKeys(paths) {
		item := mapVal(paths, p)
		for _, method := range mapKeys(item) {
			if !isHTTPMethod(method) {
				continue
			}
			ops = append(ops, OperationKey{Method: strings.ToUpper(method), Path: p})
		}
	}
	return ops, nil
}

// RenderAPI renders the API reference table and returns it alongside the
// operation set it covers. Each operation is rendered exactly once.
func RenderAPI(root string) (string, []OperationKey, error) {
	data, err := os.ReadFile(filepath.Join(root, openapiRelPath))
	if err != nil {
		return "", nil, err
	}
	rootNode, err := rootMapping(data)
	if err != nil {
		return "", nil, err
	}
	paths := mapVal(rootNode, "paths")
	if paths == nil {
		return "", nil, fmt.Errorf("docgen: openapi document has no paths")
	}
	var b strings.Builder
	var ops []OperationKey

	b.WriteString("| Method | Path | Summary |\n")
	b.WriteString("|---|---|---|\n")
	for _, p := range mapKeys(paths) {
		item := mapVal(paths, p)
		for _, method := range mapKeys(item) {
			if !isHTTPMethod(method) {
				continue
			}
			op := mapVal(item, method)
			summary := ""
			if s := mapVal(op, "summary"); s != nil {
				summary = s.Value
			} else if s := mapVal(op, "operationId"); s != nil {
				summary = s.Value
			}
			key := OperationKey{Method: strings.ToUpper(method), Path: p}
			// The row prefix MUST equal key.RowToken() so the coverage
			// test can count it — keep the two in sync.
			b.WriteString("| " + codeSpan(key.Method) + " | " + codeSpan(key.Path) +
				" | " + proseCell(summary) + " |\n")
			ops = append(ops, key)
		}
	}
	return b.String(), ops, nil
}

func generateAPIPage(root string) (string, error) {
	table, ops, err := RenderAPI(root)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(genPreamble())
	b.WriteString("\n")
	b.WriteString(crossLinkBlock())
	b.WriteString("\n")
	b.WriteString("## Operations\n\n")
	fmt.Fprintf(&b, "The v0 REST API exposes **%d operations** across the paths below, "+
		"generated from [`docs/api/v0.openapi.yaml`](https://github.com/kuhlman-labs/fishhawk/blob/main/docs/api/v0.openapi.yaml). "+
		"That document is the source of truth; this table is its published rendering.\n\n", len(ops))
	b.WriteString(table)
	return b.String(), nil
}
