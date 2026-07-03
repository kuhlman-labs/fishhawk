package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// exportMaxPages caps the continuation loop defensively. A compliance
// export at v0 volumes fits in well under this many pages even at
// --limit 1; hitting the cap means the server never reported complete —
// a bug, not a legitimate export — so the verb fails loud rather than
// spinning forever.
const exportMaxPages = 100_000

// exportPageJSON is the byte-preserving projection of one JSON export
// page. Schema and ExportedAt are kept as json.RawMessage so page 1's
// values survive verbatim; Runs' values are kept raw so each run subtree
// (whose bytes the entry hashes/signatures were computed over) is never
// re-marshalled and thus never perturbed.
type exportPageJSON struct {
	Schema     json.RawMessage            `json:"schema"`
	ExportedAt json.RawMessage            `json:"exported_at"`
	Runs       map[string]json.RawMessage `json:"runs"`
}

// mergedExportJSON is the assembled body. It carries exactly the three
// Export v1 fields so the verifier's ParseExport (DisallowUnknownFields)
// accepts it — no assembly metadata rides along.
type mergedExportJSON struct {
	Schema     json.RawMessage            `json:"schema"`
	ExportedAt json.RawMessage            `json:"exported_at"`
	Runs       map[string]json.RawMessage `json:"runs"`
}

// runExport implements `fishhawk export`: follow the header-based
// continuation of GET /v0/audit/export(.csv) to assemble ONE complete
// bounded export file. Filter mutual exclusion stays server-authoritative
// (the CLI passes --run / --repo / --from / --to through and renders the
// API error). The --out write is atomic: the whole export is assembled
// in memory first, then written (temp-file + rename for a file path) so a
// mid-pagination failure never leaves a partial file at --out.
func runExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)

	from := fs.String("from", "", "RFC3339 lower bound on run created_at (filter mode)")
	to := fs.String("to", "", "RFC3339 upper bound on run created_at (filter mode)")
	repo := fs.String("repo", "", "owner/name filter (filter mode)")
	var runIDs stringSliceFlag
	fs.Var(&runIDs, "run", "explicit run UUID to export (repeatable); mutually exclusive with --repo/--from/--to")
	limit := fs.Int("limit", 0, "max runs per page; the CLI follows the continuation across pages (0 = server default)")
	csv := fs.Bool("csv", false, "emit the flat CSV rendering instead of the JSON Export v1 body")
	out := fs.String("out", "", "write the assembled export to this path (default: stdout)")

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if len(fs.Args()) != 0 {
		_, _ = fmt.Fprintf(stderr, "fishhawk export: unexpected argument %q (this verb takes flags only)\n", fs.Args()[0])
		return exitUsage
	}

	client := newClient(cf)
	params := httpclient.ExportAuditParams{
		From:   *from,
		To:     *to,
		Repo:   *repo,
		RunIDs: runIDs,
		Limit:  *limit,
	}

	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()

	var body []byte
	var err error
	if *csv {
		body, err = assembleCSVExport(ctx, client, params)
	} else {
		body, err = assembleJSONExport(ctx, client, params)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk export: %v\n", err)
		return exitOnAPIError(err)
	}

	if err := writeExportOutput(*out, body, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk export: %v\n", err)
		return exitFailure
	}
	return exitOK
}

// fetchPage is the export-endpoint seam: it returns one page for the
// given cursor. JSON and CSV assembly share the pagination loop through
// this closure so the continuation/guard logic is written once.
type fetchPage func(ctx context.Context, cursor string) (*httpclient.ExportPage, error)

// paginate drives the header continuation: it calls fetch with an empty
// cursor, then follows NextCursor until Complete. It fails loud on a
// page that reports incomplete with no cursor (infinite-loop guard) and
// on exceeding exportMaxPages. onPage receives each page's body and
// whether it is the first page; a non-nil onPage error aborts.
func paginate(ctx context.Context, fetch fetchPage, onPage func(body []byte, first bool) error) error {
	cursor := ""
	for page := 0; ; page++ {
		if page >= exportMaxPages {
			return fmt.Errorf("export did not complete after %d pages; aborting (server never reported complete)", exportMaxPages)
		}
		p, err := fetch(ctx, cursor)
		if err != nil {
			return err
		}
		if err := onPage(p.Body, page == 0); err != nil {
			return err
		}
		if p.Complete {
			return nil
		}
		if p.NextCursor == "" {
			return errors.New("server reported the export incomplete but returned no continuation cursor; aborting rather than looping")
		}
		cursor = p.NextCursor
	}
}

// assembleJSONExport merges every JSON page into one Export v1 body.
// Each run subtree and page 1's schema/exported_at are byte-preserved;
// the runs maps are unioned. A duplicate run key across pages is a hard
// error (the server contract is disjoint pages — the global partition
// rides the first page only).
func assembleJSONExport(ctx context.Context, client *httpclient.Client, params httpclient.ExportAuditParams) ([]byte, error) {
	merged := mergedExportJSON{Runs: map[string]json.RawMessage{}}
	fetch := func(ctx context.Context, cursor string) (*httpclient.ExportPage, error) {
		p := params
		p.Cursor = cursor
		return client.ExportAudit(ctx, p)
	}
	err := paginate(ctx, fetch, func(body []byte, first bool) error {
		var page exportPageJSON
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&page); err != nil {
			return fmt.Errorf("decode export page: %w", err)
		}
		// Page 1 defines schema/exported_at for the assembled body; later
		// pages carry the same values but we keep the first verbatim.
		if first {
			merged.Schema = page.Schema
			merged.ExportedAt = page.ExportedAt
		}
		for k, v := range page.Runs {
			if _, dup := merged.Runs[k]; dup {
				return fmt.Errorf("run %q appears on more than one export page; the server contract is disjoint pages", k)
			}
			merged.Runs[k] = v
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if merged.Schema == nil {
		return nil, errors.New("export produced no pages")
	}
	assembled, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("marshal assembled export: %w", err)
	}
	return assembled, nil
}

// assembleCSVExport concatenates every CSV page: page 1 verbatim
// (including its header row), later pages with the header row stripped
// after asserting it matches page 1's. A mismatched header on a later
// page is a hard error (the pages must share one schema).
func assembleCSVExport(ctx context.Context, client *httpclient.Client, params httpclient.ExportAuditParams) ([]byte, error) {
	var buf bytes.Buffer
	var header string
	fetch := func(ctx context.Context, cursor string) (*httpclient.ExportPage, error) {
		p := params
		p.Cursor = cursor
		return client.ExportAuditCSV(ctx, p)
	}
	err := paginate(ctx, fetch, func(body []byte, first bool) error {
		if first {
			header = csvHeaderLine(body)
			buf.Write(body)
			return nil
		}
		h := csvHeaderLine(body)
		if h != header {
			return fmt.Errorf("CSV continuation page header %q does not match the first page header %q", h, header)
		}
		buf.Write(csvStripHeader(body))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// csvHeaderLine returns the first line of a CSV page (without the line
// terminator). Empty when the page has no content.
func csvHeaderLine(body []byte) string {
	if i := bytes.IndexByte(body, '\n'); i >= 0 {
		return string(bytes.TrimSuffix(body[:i], []byte("\r")))
	}
	return string(bytes.TrimSuffix(body, []byte("\r")))
}

// csvStripHeader returns body with its first line (the header row)
// removed, so continuation pages append data rows only.
func csvStripHeader(body []byte) []byte {
	if i := bytes.IndexByte(body, '\n'); i >= 0 {
		return body[i+1:]
	}
	return nil
}

// writeExportOutput writes the fully-assembled export. An empty path
// streams to stdout; a real path is written atomically (temp file in the
// same directory + rename) so a partial/failed write never lands at the
// destination path.
func writeExportOutput(path string, body []byte, stdout io.Writer) error {
	if path == "" {
		_, err := stdout.Write(body)
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".fishhawk-export-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup on any failure before the rename commits.
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename export into place: %w", err)
	}
	committed = true
	return nil
}
