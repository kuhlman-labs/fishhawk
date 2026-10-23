package issuecomment

import (
	"encoding/json"
	"regexp"
	"strings"
)

// personaNamePattern is the workflow-v2 `reviewer_personas` key rule
// (docs/spec/workflow-v2.schema.json, reviewer_personas: a key must match
// ^[a-z][a-z0-9_]*$ and a non-matching key is refused). reviewerRole derives a
// persona's role label from its name only when the name obeys this rule, so a
// tampered or future-rule payload can never inject markdown or an @-mention
// into a forge comment (E89.33 / #4669).
var personaNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// personaFallbackRole is the fixed label for a persona whose recorded name
// breaks personaNamePattern. The name itself is never echoed.
const personaFallbackRole = "Persona Reviewer"

// reviewerRole turns a recorded reviewer persona name into the role label an
// agent-review comment surface names (E89.33 / #4669). An empty persona is the
// stage's standard reviewer: "Plan Reviewer" on plan review, "PR Reviewer" on
// implement review, "Reviewer" otherwise. A declared persona is title-cased
// from its snake_case name plus " Reviewer" (architect → "Architect Reviewer",
// compliance → "Compliance Reviewer"); there is no built-in persona list
// (ADR-110), so any name the repository declares labels correctly. A trailing
// "reviewer" word is dropped when another word remains (code_reviewer → "Code
// Reviewer"), and a persona named exactly "reviewer" renders "Reviewer".
func reviewerRole(stageType, persona string) string {
	if persona == "" {
		switch stageType {
		case "plan":
			return "Plan Reviewer"
		case "implement":
			return "PR Reviewer"
		default:
			return "Reviewer"
		}
	}
	if !personaNamePattern.MatchString(persona) {
		return personaFallbackRole
	}
	var words []string
	for _, w := range strings.Split(persona, "_") {
		if w != "" {
			words = append(words, w)
		}
	}
	if len(words) > 1 && words[len(words)-1] == "reviewer" {
		words = words[:len(words)-1]
	}
	if len(words) == 1 && words[0] == "reviewer" {
		return "Reviewer"
	}
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ") + " Reviewer"
}

// reviewerPersonaOf reads the `persona` field of a review audit payload
// (*_reviewed, *_review_skipped, *_review_failed). It returns "" for an empty
// or undecodable payload and for one with no persona, i.e. a standard reviewer
// or a row recorded before the field existed.
func reviewerPersonaOf(payload []byte) string {
	var p struct {
		Persona string `json:"persona"`
	}
	// An empty, malformed or wrong-typed payload leaves Persona "": the decoder
	// validates the whole input before it sets any field.
	_ = json.Unmarshal(payload, &p)
	return p.Persona
}

// roleWithModel renders "<role> (<model>)", or just "<role>" when the model is
// unknown.
func roleWithModel(role, model string) string {
	if model == "" {
		return role
	}
	return role + " (" + model + ")"
}
