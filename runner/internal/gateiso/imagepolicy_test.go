package gateiso

import (
	"strings"
	"testing"
)

var (
	digA = "sha256:" + strings.Repeat("a", 64)
	digB = "sha256:" + strings.Repeat("b", 64)
)

func TestParseImageRef(t *testing.T) {
	ok := []struct {
		in                          string
		registry, path, tag, digest string
		pinned                      bool
		str                         string
	}{
		{"alpine", "docker.io", "library/alpine", "", "", false, "docker.io/library/alpine"},
		{"alpine:3.20", "docker.io", "library/alpine", "3.20", "", false, "docker.io/library/alpine:3.20"},
		{"org/x", "docker.io", "org/x", "", "", false, "docker.io/org/x"},
		{"docker.io/alpine", "docker.io", "library/alpine", "", "", false, "docker.io/library/alpine"},
		{"index.docker.io/org/x", "docker.io", "org/x", "", "", false, "docker.io/org/x"},
		{"ghcr.io/org/x:tag", "ghcr.io", "org/x", "tag", "", false, "ghcr.io/org/x:tag"},
		{"GHCR.io/org/x", "ghcr.io", "org/x", "", "", false, "ghcr.io/org/x"},
		{"localhost:5000/x@" + digA, "localhost:5000", "x", "", digA, true, "localhost:5000/x@" + digA},
		{"localhost/x", "localhost", "x", "", "", false, "localhost/x"},
		{"ghcr.io/org/sub/gate:v1@" + digA, "ghcr.io", "org/sub/gate", "v1", digA, true, "ghcr.io/org/sub/gate:v1@" + digA},
		{"registry:5000", "docker.io", "library/registry", "5000", "", false, "docker.io/library/registry:5000"},
		{"a.b/c__d/e-f", "a.b", "c__d/e-f", "", "", false, "a.b/c__d/e-f"},
	}
	for _, r := range ok {
		got, err := ParseImageRef(r.in)
		if err != nil {
			t.Errorf("ParseImageRef(%q) err = %v", r.in, err)
			continue
		}
		if got.Registry != r.registry || got.Path != r.path || got.Tag != r.tag || got.Digest != r.digest {
			t.Errorf("ParseImageRef(%q) = %+v", r.in, got)
		}
		if got.Pinned() != r.pinned || got.String() != r.str || got.Name() != r.registry+"/"+r.path {
			t.Errorf("ParseImageRef(%q): pinned=%v string=%q name=%q", r.in, got.Pinned(), got.String(), got.Name())
		}
	}
	bad := []struct{ in, want string }{
		{"", "empty image reference"},
		{"ghcr.io/Org/x", `invalid repository path component "Org"`},
		{"Alpine", `invalid repository path component "Alpine"`},
		{"ghcr.io/org/x@sha512:" + strings.Repeat("a", 128), "invalid digest"},
		{"ghcr.io/org/x@sha256:abc", "invalid digest"},
		{"ghcr.io/org/x@sha256:" + strings.Repeat("A", 64), "invalid digest"},
		{"ghcr.io/org/x:-bad", `invalid tag "-bad"`},
		{"ghcr.io/org/x:" + strings.Repeat("t", 129), "invalid tag"},
		{"bad_host.io/x", `invalid registry host "bad_host.io"`},
		{"ghcr.io/", "empty repository path"},
		{"ghcr.io//x", `invalid repository path component ""`},
		{"x y", "invalid repository path component"},
		{strings.Repeat("a", 513), "over the 512-byte limit"},
		{"ghcr.io/" + strings.Repeat("a", 250), "over the 255-byte limit"},
	}
	for _, r := range bad {
		_, err := ParseImageRef(r.in)
		if err == nil || !strings.Contains(err.Error(), r.want) {
			t.Errorf("ParseImageRef(%.40q) err = %v, want it to contain %q", r.in, err, r.want)
		}
	}
}

func TestParseAllowlist(t *testing.T) {
	a, err := ParseAllowlist(" ghcr.io,registry:5000\tlocalhost  localhost:5000,ghcr.io/org/ ghcr.io/org/gate\ndocker.io/library/alpine,,ghcr.io/org/gate@" + digA + " index.docker.io/alpine ")
	if err != nil {
		t.Fatal(err)
	}
	want := []AllowlistEntry{
		{Kind: AllowRegistry, Registry: "ghcr.io"},
		{Kind: AllowRegistry, Registry: "registry:5000"},
		{Kind: AllowRegistry, Registry: "localhost"},
		{Kind: AllowRegistry, Registry: "localhost:5000"},
		{Kind: AllowNamespace, Registry: "ghcr.io", Path: "org"},
		{Kind: AllowRepository, Registry: "ghcr.io", Path: "org/gate"},
		{Kind: AllowRepository, Registry: "docker.io", Path: "library/alpine"},
		{Kind: AllowDigest, Registry: "ghcr.io", Path: "org/gate", Digest: digA},
		{Kind: AllowRepository, Registry: "docker.io", Path: "library/alpine"},
	}
	if len(a) != len(want) || a.Empty() {
		t.Fatalf("got %d entries %+v, want %d", len(a), a, len(want))
	}
	for i, w := range want {
		g := a[i]
		g.Raw = ""
		if g != w {
			t.Errorf("entry %d = %+v, want %+v", i, a[i], w)
		}
	}
	if a[0].Raw != "ghcr.io" {
		t.Errorf("Raw = %q, want the entry as written", a[0].Raw)
	}
	if empty, err := ParseAllowlist(" , \t"); err != nil || !empty.Empty() {
		t.Fatalf("blank allowlist = %+v, %v; want empty", empty, err)
	}
	bad := []struct{ in, want string }{
		{"alpine", "a bare name is ambiguous: write the repository with its registry (e.g. docker.io/library/alpine)"},
		{"ghcr.io,ubuntu", `allowlist entry "ubuntu": a bare name is ambiguous`},
		{"ghcr.io/org/gate:main", "a tag is mutable and cannot be allowlisted"},
		{"ghcr.io/org/gate:main@" + digA, "a tag is mutable"},
		{"ghcr.io/*", "wildcards are not supported"},
		{"*", "wildcards are not supported"},
		{"ghcr.io/org/gate@sha256:abc", "invalid digest"},
		{"org/gate", "must name its registry explicitly"},
		{"alpine@" + digA, "must name its registry explicitly"},
		{"ghcr.io/", "a namespace entry names a registry and a namespace"},
		{"ghcr.io/org:x/", "a namespace entry carries no tag or digest"},
		{"ghcr.io/Org/", `invalid repository path component "Org"`},
		{"bad_host.io/org/", `invalid registry host "bad_host.io"`},
		{"alpine:latest", `invalid registry host "alpine:latest"`},
		{"ghcr.io/Org/gate", `invalid repository path component "Org"`},
	}
	for _, r := range bad {
		got, err := ParseAllowlist(r.in)
		if err == nil || !strings.Contains(err.Error(), r.want) || got != nil {
			t.Errorf("ParseAllowlist(%q) = %+v, %v; want error containing %q", r.in, got, err, r.want)
			continue
		}
		if !strings.Contains(err.Error(), "allowlist entry ") {
			t.Errorf("ParseAllowlist(%q) err %q does not name the entry", r.in, err)
		}
	}
}

func mustAllowlist(t *testing.T, s string) Allowlist {
	t.Helper()
	a, err := ParseAllowlist(s)
	if err != nil {
		t.Fatalf("ParseAllowlist(%q): %v", s, err)
	}
	return a
}

func mustRef(t *testing.T, s string) ImageRef {
	t.Helper()
	r, err := ParseImageRef(s)
	if err != nil {
		t.Fatalf("ParseImageRef(%q): %v", s, err)
	}
	return r
}

// TestAllowlistPermits pins each entry kind's match rule. The boundary rows
// isolate the component-boundary comparisons: a plain string prefix would
// permit them.
func TestAllowlistPermits(t *testing.T) {
	rows := []struct {
		entry, ref string
		want       bool
	}{
		{"ghcr.io", "ghcr.io/org/x:tag", true},
		{"ghcr.io", "ghcr.io.evil.example/org/x", false},
		{"ghcr.io", "quay.io/org/x", false},
		{"localhost", "localhost:5000/x", false},
		{"localhost:5000", "localhost:5000/x", true},
		{"docker.io", "alpine", true},
		{"ghcr.io/org/", "ghcr.io/org/x", true},
		{"ghcr.io/org/", "ghcr.io/org/sub/x@" + digA, true},
		{"ghcr.io/org/", "ghcr.io/organization/x", false},
		{"ghcr.io/org/", "ghcr.io/org", false},
		{"ghcr.io/org/", "quay.io/org/x", false},
		{"ghcr.io/org/gate", "ghcr.io/org/gate:any", true},
		{"ghcr.io/org/gate", "ghcr.io/org/gate@" + digA, true},
		{"ghcr.io/org/gate", "ghcr.io/org/gateway", false},
		{"ghcr.io/org/gate", "ghcr.io/org/gate/sub", false},
		{"docker.io/library/alpine", "alpine:3", true},
		{"docker.io/alpine", "index.docker.io/library/alpine", true},
		{"ghcr.io/org/gate@" + digA, "ghcr.io/org/gate@" + digA, true},
		{"ghcr.io/org/gate@" + digA, "ghcr.io/org/gate:v1@" + digA, true},
		{"ghcr.io/org/gate@" + digA, "ghcr.io/org/gate@" + digB, false},
		{"ghcr.io/org/gate@" + digA, "ghcr.io/org/gate:v1", false},
		{"ghcr.io/org/gate@" + digA, "ghcr.io/org/other@" + digA, false},
	}
	for _, r := range rows {
		if got := mustAllowlist(t, r.entry).Permits(mustRef(t, r.ref)); got != r.want {
			t.Errorf("allowlist %q permits %q = %v, want %v", r.entry, r.ref, got, r.want)
		}
	}
	if (Allowlist{}).Permits(mustRef(t, "alpine")) {
		t.Error("an empty allowlist permits nothing by itself")
	}
	if (AllowlistEntry{Kind: "bogus", Registry: "docker.io"}).permits(mustRef(t, "alpine")) {
		t.Error("an unknown entry kind must permit nothing")
	}
	multi := mustAllowlist(t, "quay.io ghcr.io/org/gate")
	if !multi.Permits(mustRef(t, "ghcr.io/org/gate:x")) || multi.Permits(mustRef(t, "ghcr.io/org/other")) {
		t.Error("a multi-entry allowlist must permit by any entry and nothing else")
	}
}

func TestParseBuildPolicy(t *testing.T) {
	rows := []struct {
		value   string
		profile Profile
		want    bool
	}{
		{"", ProfileLocal, true},
		{"", ProfileSelfHosted, true},
		{"", ProfileHosted, false},
		{"", Profile("bogus"), false},
		{"allow", ProfileHosted, true},
		{" ALLOW ", ProfileHosted, true},
		{"deny", ProfileLocal, false},
		{"Deny", ProfileSelfHosted, false},
	}
	for _, r := range rows {
		got, err := ParseBuildPolicy(r.value, r.profile)
		if err != nil || got != r.want {
			t.Errorf("ParseBuildPolicy(%q, %s) = %v, %v; want %v", r.value, r.profile, got, err, r.want)
		}
	}
	_, err := ParseBuildPolicy("maybe", ProfileLocal)
	if err == nil || !strings.Contains(err.Error(), `"maybe"`) || !strings.Contains(err.Error(), "allow, deny") {
		t.Fatalf("ParseBuildPolicy(maybe) err = %v", err)
	}
}

func TestDeclaredSource(t *testing.T) {
	for s, want := range map[string]bool{ImageSourceStage: true, ImageSourceWorkflow: true, ImageSourceEnv: false, "": false, "bogus": false} {
		if DeclaredSource(s) != want {
			t.Errorf("DeclaredSource(%q) = %v, want %v", s, !want, want)
		}
	}
}

// TestEvaluateImagePolicy is one row per branch per profile. Each refusal
// fixture violates ONLY its own branch's rule, so mutating that branch to
// allow turns exactly its rows red: the off-list rows use a PINNED ref (the
// tag-only branch cannot mask them), the hosted empty-allowlist rows use a
// pinned ref, the hosted tag-only row uses a PERMITTING allowlist (the
// allowlist branches cannot mask it), and the hosted build empty-allowlist
// row sets buildAllowed (the disabled-build branch cannot mask it).
func TestEvaluateImagePolicy(t *testing.T) {
	pinned := "ghcr.io/org/gate@" + digA
	tagOnly := "ghcr.io/org/gate:main"
	permit := mustAllowlist(t, "ghcr.io/org/")
	offList := mustAllowlist(t, "quay.io")
	stage := func(img string) ImageRequest { return ImageRequest{Source: ImageSourceStage, Image: img} }
	build := ImageRequest{Source: ImageSourceWorkflow, Dockerfile: "gate/Dockerfile", Context: "gate"}
	type want struct {
		allowed       bool
		refusal       string
		warning       string
		ref           string
		build         bool
		basesMustPass bool
		pinnedBases   bool
	}
	type row struct {
		name         string
		profile      Profile
		req          ImageRequest
		allow        Allowlist
		buildAllowed bool
		want         want
	}
	var rows []row
	all := []Profile{ProfileLocal, ProfileSelfHosted, ProfileHosted}
	for _, p := range all {
		strict := p == ProfileHosted
		ps := string(p)
		rows = append(rows,
			row{"none/" + ps, p, ImageRequest{}, nil, false, want{allowed: true}},
			row{"env-unchecked/" + ps, p, ImageRequest{Source: ImageSourceEnv, Image: "not a ref:::"}, offList, false, want{allowed: true}},
			row{"env-with-build/" + ps, p, ImageRequest{Source: ImageSourceEnv, Dockerfile: "Dockerfile", Context: "."}, nil, true, want{refusal: "only a declared gate_container can build"}},
			row{"unknown-source/" + ps, p, ImageRequest{Image: pinned}, permit, true, want{refusal: `unknown source ""`}},
			row{"pinned-permitting/" + ps, p, stage(pinned), permit, false, want{allowed: true, ref: pinned}},
			row{"off-list/" + ps, p, stage(pinned), offList, false, want{refusal: "gate_container (stage) image " + pinned + " is not permitted by the operator image allowlist (FISHHAWK_GATE_IMAGE_ALLOWLIST)"}},
			row{"unparsable/" + ps, p, stage("ghcr.io/Org/x"), permit, false, want{refusal: "gate_container (stage) image is not a valid image reference"}},
			row{"both-sources/" + ps, p, ImageRequest{Source: ImageSourceStage, Image: pinned, Dockerfile: "Dockerfile", Context: "."}, permit, true, want{refusal: "declares both image and dockerfile/context"}},
			row{"image-and-context/" + ps, p, ImageRequest{Source: ImageSourceStage, Image: pinned, Context: "."}, permit, true, want{refusal: "declares both image and dockerfile/context"}},
			row{"unpaired-dockerfile/" + ps, p, ImageRequest{Source: ImageSourceWorkflow, Dockerfile: "Dockerfile"}, permit, true, want{refusal: "must declare dockerfile and context together"}},
			row{"unpaired-context/" + ps, p, ImageRequest{Source: ImageSourceWorkflow, Context: "."}, permit, true, want{refusal: "must declare dockerfile and context together"}},
			row{"declared-nothing/" + ps, p, ImageRequest{Source: ImageSourceStage}, permit, true, want{refusal: "must declare dockerfile and context together"}},
			row{"dotdot-dockerfile/" + ps, p, ImageRequest{Source: ImageSourceStage, Dockerfile: "../x/Dockerfile", Context: "."}, permit, true, want{refusal: `dockerfile "../x/Dockerfile" escapes the repository`}},
			row{"dotdot-context/" + ps, p, ImageRequest{Source: ImageSourceStage, Dockerfile: "Dockerfile", Context: "a/../.."}, permit, true, want{refusal: `context "a/../.." escapes the repository`}},
			row{"absolute-dockerfile/" + ps, p, ImageRequest{Source: ImageSourceStage, Dockerfile: "/abs/Dockerfile", Context: "."}, permit, true, want{refusal: `dockerfile "/abs/Dockerfile" is not a repo-relative path`}},
			row{"long-context/" + ps, p, ImageRequest{Source: ImageSourceStage, Dockerfile: "Dockerfile", Context: strings.Repeat("a", 1025)}, permit, true, want{refusal: "context is 1025 bytes, over the 1024-byte limit"}},
			row{"build-disabled/" + ps, p, build, permit, false, want{refusal: "gate_container (workflow) refused: in-repo gate image builds are disabled under profile " + ps + " (FISHHAWK_GATE_BUILD)"}},
			row{"build-allowlist/" + ps, p, build, permit, true, want{allowed: true, build: true, basesMustPass: true, pinnedBases: strict}},
		)
		if strict {
			rows = append(rows,
				row{"pinned-empty-allowlist/" + ps, p, stage(pinned), nil, false, want{refusal: "profile=hosted permits a declared gate image only through an operator image allowlist (FISHHAWK_GATE_IMAGE_ALLOWLIST is empty)"}},
				row{"tag-only/" + ps, p, stage(tagOnly), permit, false, want{refusal: "gate_container (stage) image " + tagOnly + " is not digest-pinned: profile=hosted requires ghcr.io/org/gate@sha256:<digest>"}},
				row{"build-empty-allowlist/" + ps, p, build, nil, true, want{refusal: "profile=hosted permits an in-repo gate image build only with an operator image allowlist for its bases"}},
			)
		} else {
			rows = append(rows,
				row{"pinned-empty-allowlist/" + ps, p, stage(pinned), nil, false, want{allowed: true, ref: pinned}},
				row{"tag-only/" + ps, p, stage(tagOnly), nil, false, want{allowed: true, ref: tagOnly, warning: "gate_container (stage) image " + tagOnly + " is not digest-pinned"}},
				row{"build-empty-allowlist/" + ps, p, build, nil, true, want{allowed: true, build: true}},
				row{"build-context-dot/" + ps, p, ImageRequest{Source: ImageSourceStage, Dockerfile: "./ci/Dockerfile", Context: "."}, nil, true, want{allowed: true, build: true}},
			)
		}
	}
	// An unrecognised profile gets the hosted posture (fail closed).
	rows = append(rows,
		row{"tag-only/unknown-profile", Profile("bogus"), stage(tagOnly), permit, false, want{refusal: "is not digest-pinned: profile=bogus"}},
		row{"pinned-empty-allowlist/unknown-profile", Profile("bogus"), stage(pinned), nil, false, want{refusal: "profile=bogus permits a declared gate image only through"}},
	)
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			d := EvaluateImagePolicy(r.profile, r.req, r.allow, r.buildAllowed)
			if d.Allowed != r.want.allowed {
				t.Fatalf("Allowed = %v, want %v (decision %+v)", d.Allowed, r.want.allowed, d)
			}
			if d.Allowed == (d.Refusal != "") {
				t.Fatalf("Allowed=%v with Refusal %q: exactly one must hold", d.Allowed, d.Refusal)
			}
			if r.want.refusal != "" && !strings.Contains(d.Refusal, r.want.refusal) {
				t.Fatalf("Refusal = %q, want it to contain %q", d.Refusal, r.want.refusal)
			}
			if (r.want.warning == "") != (d.Warning == "") || !strings.Contains(d.Warning, r.want.warning) {
				t.Fatalf("Warning = %q, want %q", d.Warning, r.want.warning)
			}
			gotRef := ""
			if d.Ref != (ImageRef{}) {
				gotRef = d.Ref.String()
			}
			if gotRef != r.want.ref {
				t.Fatalf("Ref = %q, want %q", gotRef, r.want.ref)
			}
			if d.Build != r.want.build || d.BasesMustPass != r.want.basesMustPass || d.RequirePinnedBases != r.want.pinnedBases {
				t.Fatalf("Build/BasesMustPass/RequirePinnedBases = %v/%v/%v, want %v/%v/%v", d.Build, d.BasesMustPass, d.RequirePinnedBases, r.want.build, r.want.basesMustPass, r.want.pinnedBases)
			}
		})
	}
}
