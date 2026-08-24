package run

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// childParamsParentFixture builds a parent *Run with EVERY field the
// inheritance table names set to a distinctive NON-ZERO value —
// including the notInherited ones. That is the vacuousness guard for
// the two behavioral assertions in TestChildParamsFrom_TableModesMatchBehavior:
// an `inherited` equality assertion against a zero parent field would
// pass even if the helper never copied it, and a `notInherited` zero
// assertion would pass merely because the parent was zero too.
//
// ParentRunID deliberately points at a DIFFERENT uuid than parent.ID so
// `derived` is distinguishable from `inherited` — a helper that copied
// parent.ParentRunID verbatim (the grandparent bug) would otherwise be
// indistinguishable from one deriving &parent.ID.
func childParamsParentFixture(t *testing.T) *Run {
	t.Helper()
	triggerRef := "issue:2589"
	installID := int64(4242)
	installRef := "gitlab:2589"
	idemKey := "parent-idempotency-key"
	grandparent := uuid.New()
	upstream := uuid.New()
	decomposedFrom := uuid.New()
	sliceIdx := 3
	return &Run{
		ID:              uuid.New(),
		Repo:            "kuhlman-labs/fishhawk",
		WorkflowID:      "feature_change",
		WorkflowSHA:     "feedf00dcafe",
		TriggerSource:   TriggerGitHubIssue,
		TriggerRef:      &triggerRef,
		InstallationID:  &installID,
		InstallationRef: &installRef,
		IdempotencyKey:  &idemKey,
		ParentRunID:     &grandparent,
		UpstreamRunID:   &upstream,
		RequiredChecksSnapshot: &RequiredChecksSnapshot{
			Contexts: []string{"ci/build"},
			Sources:  []string{"branch_protection"},
		},
		WorkflowSpec:       []byte("version: \"2\"\n"),
		RetryAttempt:       7,
		MaxRetriesSnapshot: 5,
		RunnerKind:         RunnerKindLocal,
		IssueContext: &IssueContext{
			Number: 2589,
			Title:  "Inherit run-from-run fields by construction",
			Body:   "the parent's cached issue body",
			URL:    "https://github.com/kuhlman-labs/fishhawk/issues/2589",
		},
		DecomposedFrom: &decomposedFrom,
		Drive:          true,
		SliceIndex:     &sliceIdx,
		WorkingDir:     "/tmp/fishhawk-parent-checkout",
	}
}

// TestChildParamsFixture_EveryFieldNonZero is the vacuousness guard: for
// every CreateRunParams field the table names, the shared parent fixture
// must carry a non-zero same-named Run field. Zeroing one turns this red
// BEFORE it can silently make a mode assertion pass for the wrong reason.
func TestChildParamsFixture_EveryFieldNonZero(t *testing.T) {
	parent := childParamsParentFixture(t)
	pv := reflect.ValueOf(*parent)
	pt := pv.Type()

	paramsType := reflect.TypeOf(CreateRunParams{})
	for i := range paramsType.NumField() {
		name := paramsType.Field(i).Name
		if _, ok := pt.FieldByName(name); !ok {
			t.Errorf("CreateRunParams field %q has no same-named Run field:"+
				" the fixture cannot seed it, so no mode assertion for it can be trusted", name)
			continue
		}
		fv := pv.FieldByName(name)
		if fv.IsZero() {
			t.Errorf("parent fixture field %q is the zero value: an inherited-equality or"+
				" notInherited-zero assertion on it would pass vacuously", name)
		}
	}
}

// TestChildParamsFrom_InheritanceTableCoversEveryField is the two-sided
// schema pin. Adding a CreateRunParams field without deciding its
// inheritance fails here naming the field; a table row left dangling by
// a rename fails here too.
func TestChildParamsFrom_InheritanceTableCoversEveryField(t *testing.T) {
	paramsType := reflect.TypeOf(CreateRunParams{})

	fields := make(map[string]bool, paramsType.NumField())
	for i := range paramsType.NumField() {
		name := paramsType.Field(i).Name
		fields[name] = true
		decision, ok := childParamsInheritance[name]
		if !ok {
			t.Errorf("CreateRunParams field %q has no childParamsInheritance row:"+
				" decide whether a run minted from another run inherits it, and state why", name)
			continue
		}
		switch decision.Mode {
		case modeInherited, modeDerived, modeNotInherited:
		default:
			t.Errorf("field %q has unknown inheritance mode %q", name, decision.Mode)
		}
		if decision.Reason == "" {
			t.Errorf("field %q has an empty Reason: the reason IS the reviewable decision record", name)
		}
	}

	for name := range childParamsInheritance {
		if !fields[name] {
			t.Errorf("childParamsInheritance has a row for %q, which is not a CreateRunParams field:"+
				" a rename left the decision record dangling", name)
		}
	}
}

// TestChildParamsFrom_TableModesMatchBehavior makes the table binding
// rather than decorative: each mode is asserted against what
// ChildParamsFrom actually returns, driven off the guarded fixture.
func TestChildParamsFrom_TableModesMatchBehavior(t *testing.T) {
	parent := childParamsParentFixture(t)
	got := ChildParamsFrom(parent)

	gv := reflect.ValueOf(got)
	pv := reflect.ValueOf(*parent)
	paramsType := gv.Type()

	for i := range paramsType.NumField() {
		name := paramsType.Field(i).Name
		decision, ok := childParamsInheritance[name]
		if !ok {
			// A missing row must FAIL here, not be skipped: deleting a
			// field's row AND its copy from the helper would otherwise
			// silently disable this test's behavioral assertion for it
			// and leave the deletion green in this test.
			t.Errorf("field %q has no childParamsInheritance row, so its behavior is unasserted:"+
				" add a row rather than removing the field from the decision record", name)
			continue
		}
		child := gv.Field(i).Interface()

		switch decision.Mode {
		case modeInherited:
			want := pv.FieldByName(name).Interface()
			if !reflect.DeepEqual(child, want) {
				t.Errorf("field %q is table-mode inherited but ChildParamsFrom returned %#v, want the parent's %#v",
					name, child, want)
			}
		case modeNotInherited:
			if !gv.Field(i).IsZero() {
				t.Errorf("field %q is table-mode notInherited but ChildParamsFrom returned non-zero %#v",
					name, child)
			}
		case modeDerived:
			// An unrecognised derived field FAILS rather than being
			// skipped: a new derived row must not ship unasserted.
			switch name {
			case "ParentRunID":
				if got.ParentRunID == nil {
					t.Fatalf("ParentRunID is derived but came back nil")
				}
				if *got.ParentRunID != parent.ID {
					t.Errorf("ParentRunID = %s, want &parent.ID (%s)", *got.ParentRunID, parent.ID)
				}
				if parent.ParentRunID != nil && *got.ParentRunID == *parent.ParentRunID {
					t.Errorf("ParentRunID was copied from the parent's own ParentRunID (%s):"+
						" that points the child at its grandparent", *parent.ParentRunID)
				}
			default:
				t.Errorf("field %q is table-mode derived but this test has no assertion for it:"+
					" add one rather than letting a derived field ship unasserted", name)
			}
		}
	}
}

// TestChildParamsFrom_NilParent pins the defensive nil branch: these
// call sites sit in HTTP handlers and webhook dispatch, where a
// nil-deref would 500 the whole request.
func TestChildParamsFrom_NilParent(t *testing.T) {
	got := ChildParamsFrom(nil)
	if !reflect.DeepEqual(got, CreateRunParams{}) {
		t.Errorf("ChildParamsFrom(nil) = %#v, want the zero CreateRunParams", got)
	}
}

// TestChildParamsFrom_InheritsInstallationRef is the named failure mode behind
// the InstallationRef row in the inheritance table (E45.22 / #2043): a run
// minted from a gitlab_ci parent must keep the parent's GitLab credential ref.
// A child that dropped it would fall back to an InstallationID it does not
// have and resolve the zero credential scope, so every one of its stages would
// warn-skip.
//
// The reflection pin in TestChildParamsFrom_TableModesMatchBehavior already
// checks the MODE agrees with the helper; this test names the consequence, and
// covers the nil case the fixture (which is always non-nil) cannot.
func TestChildParamsFrom_InheritsInstallationRef(t *testing.T) {
	ref := "gitlab:5"
	parent := &Run{
		ID:              uuid.New(),
		Repo:            "gitlab-org/gitlab-test",
		WorkflowID:      "feature_change",
		RunnerKind:      RunnerKindGitLabCI,
		InstallationRef: &ref,
	}
	child := ChildParamsFrom(parent)
	if child.InstallationRef == nil {
		t.Fatal("child dropped the parent's InstallationRef; a gitlab_ci retry would lose its credentials")
	}
	if *child.InstallationRef != ref {
		t.Errorf("child InstallationRef = %q, want %q", *child.InstallationRef, ref)
	}

	// A parent with no ref hands the child no ref — nil is inherited as nil,
	// not manufactured into an empty string (the two are distinct persisted
	// states).
	noRef := ChildParamsFrom(&Run{ID: uuid.New(), Repo: "x/y"})
	if noRef.InstallationRef != nil {
		t.Errorf("child InstallationRef = %q, want nil when the parent has none", *noRef.InstallationRef)
	}
}
