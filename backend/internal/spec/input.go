package spec

// StageDeclaresPullRequestInput recognizes both external PR inputs and PR
// artifacts handed off by a preceding stage. For a review, either denotes a landing-managed
// review gate rather than an operator-confirmation gate.
func StageDeclaresPullRequestInput(st Stage) bool {
	for _, in := range st.Inputs {
		if in.Source == InputSourcePullRequest || in.Artifact == string(ArtifactPullRequest) {
			return true
		}
	}
	return false
}
