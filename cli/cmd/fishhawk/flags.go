package main

import "flag"

// parseIntermixed parses args against fs, collecting positional arguments
// that halt the standard flag parser and resuming on the remaining args so
// that flags and positionals may appear in any order.
//
// Relies on flag.FlagSet.Parse halting (without returning an error) at the
// first non-flag argument and populating FlagSet.Args() with that argument
// and everything that follows. The loop continues while the first remaining
// argument looks like a positional (does not start with '-').
func parseIntermixed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		remaining := fs.Args()
		if len(remaining) == 0 || remaining[0][0] == '-' {
			break
		}
		positionals = append(positionals, remaining[0])
		args = remaining[1:]
	}
	return positionals, nil
}
