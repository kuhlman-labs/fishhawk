package permdrift

import (
	"bytes"
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

// defaultTokenValue is the Value of the entry a job with NO permissions block
// anywhere (job or workflow level) receives. GitHub documents the default
// GITHUB_TOKEN as either permissive (read/write) or restricted per
// repository/organization setting, which this file cannot see — so the check
// models the default CONSERVATIVELY as write on every scope. Removing a block
// therefore always reads as a widening and adding one as a narrowing.
const defaultTokenValue = "write (default token permissions)"

// ExtractActions extracts the effective GITHUB_TOKEN permissions of a GitHub
// Actions workflow file. Each job's effective block is
// jobs.<id>.permissions, else the top-level permissions, else the DEFAULT:
//
//   - `write-all` -> `jobs.<id>.*` = write; `read-all` -> `jobs.<id>.*` = read
//   - DEFAULT (no block at either level, or a null one) -> `jobs.<id>.*` =
//     write (see defaultTokenValue)
//   - `{}` -> no grants
//   - a map -> `jobs.<id>.<scope>` per scope at its level; `none` is no grant
//
// A file declaring no jobs keys the top-level block as `permissions.<scope>`
// (and `permissions.*` for read-all/write-all). Every entry is a ranked Grant
// on ActionsLevels. Empty content (an absent file) yields an empty set; YAML
// that does not parse, a non-mapping document or jobs entry, and an
// unrecognized level are errors.
func ExtractActions(content []byte) (Grants, error) {
	out := Grants{}
	if len(bytes.TrimSpace(content)) == 0 {
		return out, nil
	}
	var doc map[string]any
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, fmt.Errorf("actions workflow: %w", err)
	}
	topBlock, topPresent := doc["permissions"]
	if topBlock == nil {
		topPresent = false
	}

	jobsRaw, hasJobs := doc["jobs"]
	jobs, ok := jobsRaw.(map[string]any)
	if hasJobs && jobsRaw != nil && !ok {
		return nil, fmt.Errorf("actions workflow: jobs is not a mapping")
	}
	if len(jobs) == 0 {
		if !topPresent {
			return out, nil
		}
		if err := addActionsBlock(out, "permissions", topBlock); err != nil {
			return nil, err
		}
		return out, nil
	}

	ids := make([]string, 0, len(jobs))
	for id := range jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		job, ok := jobs[id].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("actions workflow: job %q is not a mapping", id)
		}
		prefix := "jobs." + id
		block, present := job["permissions"]
		if block == nil {
			present = false
		}
		switch {
		case present:
			if err := addActionsBlock(out, prefix, block); err != nil {
				return nil, err
			}
		case topPresent:
			if err := addActionsBlock(out, prefix, topBlock); err != nil {
				return nil, err
			}
		default:
			out.Put(Entry{Key: prefix + WildcardSuffix, Value: defaultTokenValue, Rank: actionsWrite, Polarity: Grant})
		}
	}
	return out, nil
}

// actionsWrite is ActionsLevels' rank for write.
var actionsWrite, _ = ActionsLevels.Rank("write")

// addActionsBlock records one permissions block under prefix.
func addActionsBlock(out Grants, prefix string, block any) error {
	switch b := block.(type) {
	case string:
		switch b {
		case "write-all":
			out.Put(Entry{Key: prefix + WildcardSuffix, Value: "write (write-all)", Rank: actionsWrite, Polarity: Grant})
		case "read-all":
			r, _ := ActionsLevels.Rank("read")
			out.Put(Entry{Key: prefix + WildcardSuffix, Value: "read (read-all)", Rank: r, Polarity: Grant})
		default:
			return fmt.Errorf("actions workflow: %s: unrecognized permissions shorthand %q (want read-all, write-all or a mapping)", prefix, b)
		}
	case map[string]any:
		for scope, lv := range b {
			level, ok := lv.(string)
			if !ok {
				return fmt.Errorf("actions workflow: %s.%s: level is not a string", prefix, scope)
			}
			r, err := ActionsLevels.mustRank(prefix+"."+scope, level)
			if err != nil {
				return fmt.Errorf("actions workflow: %w", err)
			}
			if r == 0 {
				// `none` grants nothing: identical to the scope being
				// omitted from a declared block.
				continue
			}
			out.Put(Entry{Key: prefix + "." + scope, Value: level, Rank: r, Polarity: Grant})
		}
	default:
		return fmt.Errorf("actions workflow: %s: permissions is neither a shorthand string nor a mapping", prefix)
	}
	return nil
}
