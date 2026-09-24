package workmgmt

import "strings"

// RunnableLabelPrefix is the label namespace an issue DECLARES, in advance, that
// it produces no diff (#3649): `runnable:no`. It is a fixed constant rather than
// a work-management conventions key, following the AutonomyLabelPrefix
// precedent — the conventions schema is additionalProperties:false, so a
// configurable namespace would drag the canonical schema and its mirrors into
// scope for a name.
const RunnableLabelPrefix = "runnable:"

// ParseRunnableLabel reports whether an issue's labels DECLARE it not runnable:
// true only when the first `runnable:<value>` label carries the recognized
// suffix "no". `runnable:yes`, an unrecognized suffix (a typo like
// "runnable:maybe"), an empty suffix and an absent label all degrade to false =
// runnable — the fail-SAFE direction for the advisory campaign admission
// screen that reads it: a mislabeled item is reported as runnable, never
// silently flagged. It is a DECLARATION parse, not an inference: nothing derives
// it from issue content. It is the single source of truth for the namespace,
// shared by both github EpicChild construction sites via the provider's
// delegate, mirroring ParseAutonomyLabel.
func ParseRunnableLabel(labels []string) bool {
	for _, l := range labels {
		if v := strings.TrimPrefix(l, RunnableLabelPrefix); v != l {
			return v == "no"
		}
	}
	return false
}
