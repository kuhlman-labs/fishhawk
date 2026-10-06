package gateiso

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": ModeAuto, "auto": ModeAuto, " Container ": ModeContainer, "clone-sandbox": ModeCloneSandbox, "clone": ModeClone} {
		got, err := ParseMode(in)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	_, err := ParseMode("sandbox")
	if err == nil || !strings.Contains(err.Error(), `"sandbox"`) || !strings.Contains(err.Error(), "auto, container, clone-sandbox, clone") {
		t.Fatalf("ParseMode(sandbox) err = %v", err)
	}
}

func TestParseProfile(t *testing.T) {
	for in, want := range map[string]Profile{"": ProfileLocal, "local": ProfileLocal, "SELF-HOSTED": ProfileSelfHosted, "hosted": ProfileHosted} {
		got, err := ParseProfile(in)
		if err != nil || got != want {
			t.Errorf("ParseProfile(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	_, err := ParseProfile("cloud")
	if err == nil || !strings.Contains(err.Error(), `"cloud"`) || !strings.Contains(err.Error(), "local, self-hosted, hosted") {
		t.Fatalf("ParseProfile(cloud) err = %v", err)
	}
}

var (
	safeRT   = Runtime{Kind: KindDocker, Safe: true, Reason: "docker 27 over local unix socket /s", SocketPath: "/s"}
	unsafeRT = Runtime{Kind: KindDocker, Safe: false, Reason: "docker endpoint from DOCKER_HOST is not a local unix socket: scheme tcp"}
	noRT     = Runtime{Kind: KindNone, Reason: "no container runtime on PATH (docker or podman)"}
	sbOK     = SandboxProbe{Available: true}
	sbNo     = SandboxProbe{Available: false, Reason: "unshare unavailable on darwin (ADR-063 gap)"}
)

// TestSelect_Table is the full mode × profile × runtime × image × sandbox
// table. Deleting the profile branch in Select turns every hosted-refused
// row red.
func TestSelect_Table(t *testing.T) {
	type row struct {
		name    string
		mode    Mode
		profile Profile
		image   string
		rt      Runtime
		sb      SandboxProbe
		want    Path
		reason  string
	}
	rows := []row{}
	// Container available → container under every mode except explicit fallbacks.
	for _, p := range profiles {
		rows = append(rows,
			row{"auto/safe+image/" + string(p), ModeAuto, p, "img", safeRT, sbNo, PathContainer, "container path available"},
			row{"container/safe+image/" + string(p), ModeContainer, p, "img", safeRT, sbOK, PathContainer, "mode=container"},
			row{"container/safe-noimage/" + string(p), ModeContainer, p, "", safeRT, sbOK, PathRefused, "no gate image configured (FISHHAWK_GATE_IMAGE is empty)"},
			row{"container/unsafe+image/" + string(p), ModeContainer, p, "img", unsafeRT, sbOK, PathRefused, "docker is not a safe runtime (docker endpoint from DOCKER_HOST"},
			row{"container/none+image/" + string(p), ModeContainer, p, "img", noRT, sbOK, PathRefused, "no safe container runtime (no container runtime on PATH"},
		)
	}
	// Non-hosted fallbacks.
	for _, p := range []Profile{ProfileLocal, ProfileSelfHosted} {
		rows = append(rows,
			row{"auto/unsafe/sandbox/" + string(p), ModeAuto, p, "img", unsafeRT, sbOK, PathCloneSandbox, "falling back to clone-sandbox"},
			row{"auto/unsafe/nosandbox/" + string(p), ModeAuto, p, "img", unsafeRT, sbNo, PathClone, "falling back to clone"},
			row{"auto/safe-noimage/nosandbox/" + string(p), ModeAuto, p, "", safeRT, sbNo, PathClone, "no gate image configured"},
			row{"auto/none/nosandbox/" + string(p), ModeAuto, p, "", noRT, sbNo, PathClone, "no safe container runtime"},
			row{"clone-sandbox/sandbox/" + string(p), ModeCloneSandbox, p, "img", safeRT, sbOK, PathCloneSandbox, "sandbox available"},
			row{"clone-sandbox/nosandbox/" + string(p), ModeCloneSandbox, p, "img", safeRT, sbNo, PathRefused, "sandbox is unavailable: unshare unavailable on darwin"},
			row{"clone/" + string(p), ModeClone, p, "img", safeRT, sbOK, PathClone, "mode=clone"},
		)
	}
	// Hosted refuses every non-container path.
	rows = append(rows,
		row{"auto/unsafe/sandbox/hosted", ModeAuto, ProfileHosted, "img", unsafeRT, sbOK, PathRefused, "profile=hosted refuses every non-container path"},
		row{"auto/unsafe/nosandbox/hosted", ModeAuto, ProfileHosted, "img", unsafeRT, sbNo, PathRefused, "docker is not a safe runtime"},
		row{"auto/safe-noimage/hosted", ModeAuto, ProfileHosted, "", safeRT, sbOK, PathRefused, "no gate image configured"},
		row{"auto/none-noimage/hosted", ModeAuto, ProfileHosted, "", noRT, sbOK, PathRefused, "no safe container runtime (no container runtime on PATH (docker or podman)); no gate image configured"},
		row{"clone-sandbox/sandbox/hosted", ModeCloneSandbox, ProfileHosted, "img", safeRT, sbOK, PathRefused, "profile=hosted refuses mode=clone-sandbox"},
		row{"clone/hosted", ModeClone, ProfileHosted, "img", safeRT, sbOK, PathRefused, "profile=hosted refuses mode=clone"},
	)
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			in := Inputs{Mode: r.mode, Profile: r.profile, Image: r.image, Runtime: r.rt, Sandbox: r.sb}
			sel := Select(in)
			if sel.Path != r.want {
				t.Fatalf("path = %q, want %q (reason %q)", sel.Path, r.want, sel.Reason)
			}
			if !strings.Contains(sel.Reason, r.reason) {
				t.Fatalf("reason %q does not contain %q", sel.Reason, r.reason)
			}
			if sel.Refused() != (r.want == PathRefused) {
				t.Fatalf("Refused() = %v for path %q", sel.Refused(), sel.Path)
			}
			if sel.Mode != r.mode || sel.Profile != r.profile || sel.Image != r.image || sel.Runtime != r.rt || sel.Sandbox != r.sb {
				t.Fatalf("selection does not echo its inputs: %+v", sel)
			}
		})
	}
}

func TestSelect_UnknownModeRefused(t *testing.T) {
	sel := Select(Inputs{Mode: "bogus", Profile: ProfileLocal, Image: "img", Runtime: safeRT})
	if sel.Path != PathRefused || !strings.Contains(sel.Reason, `unknown isolation mode "bogus"`) {
		t.Fatalf("sel = %+v", sel)
	}
}

func TestSelect_IsPureAndJSONRecordable(t *testing.T) {
	in := Inputs{Mode: ModeAuto, Profile: ProfileHosted, Image: "", Runtime: unsafeRT, Sandbox: sbOK}
	a, b := Select(in), Select(in)
	if a != b {
		t.Fatalf("Select is not deterministic: %+v vs %+v", a, b)
	}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"path":"refused"`, `"mode":"auto"`, `"profile":"hosted"`, `"runtime":{"kind":"docker","safe":false`, `"endpoint":{`, `"sandbox":{"available":true}`, `"reason":"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("selection JSON %s lacks %s", raw, key)
		}
	}
}

func TestProfileForbidsFallback(t *testing.T) {
	for _, m := range modes {
		for _, p := range []Profile{ProfileLocal, ProfileSelfHosted} {
			if err := ProfileForbidsFallback(p, m); err != nil {
				t.Errorf("%s/%s: unexpected %v", p, m, err)
			}
		}
	}
	for _, m := range []Mode{ModeAuto, ModeContainer} {
		if err := ProfileForbidsFallback(ProfileHosted, m); err != nil {
			t.Errorf("hosted/%s: unexpected %v", m, err)
		}
	}
	for _, m := range []Mode{ModeClone, ModeCloneSandbox} {
		err := ProfileForbidsFallback(ProfileHosted, m)
		if err == nil || !strings.Contains(err.Error(), string(m)) || !strings.Contains(err.Error(), "hosted") {
			t.Errorf("hosted/%s: err = %v", m, err)
		}
	}
}

// TestSelect_ContainerUnavailable pins that every non-container outcome names
// why the container path was not taken (#2135), and the container path names
// nothing. The auto/no-runtime/no-image row asserts both missing pieces: the
// fixture has neither, so Select's own assignment is the only source of the
// asserted substrings (Reason is not consulted).
func TestSelect_ContainerUnavailable(t *testing.T) {
	rows := []struct {
		name    string
		in      Inputs
		path    Path
		want    []string
		wantNil bool
	}{
		{"auto/none-noimage/local → clone", Inputs{Mode: ModeAuto, Profile: ProfileLocal, Runtime: noRT, Sandbox: sbNo}, PathClone,
			[]string{"no safe container runtime (no container runtime on PATH", "no gate image configured (FISHHAWK_GATE_IMAGE is empty)"}, false},
		{"auto/unsafe/sandbox → clone-sandbox", Inputs{Mode: ModeAuto, Profile: ProfileSelfHosted, Image: "img", Runtime: unsafeRT, Sandbox: sbOK}, PathCloneSandbox,
			[]string{"docker is not a safe runtime"}, false},
		{"auto/hosted → refused", Inputs{Mode: ModeAuto, Profile: ProfileHosted, Image: "img", Runtime: unsafeRT, Sandbox: sbOK}, PathRefused,
			[]string{"docker is not a safe runtime"}, false},
		{"container/unavailable → refused", Inputs{Mode: ModeContainer, Profile: ProfileLocal, Runtime: safeRT, Sandbox: sbOK}, PathRefused,
			[]string{"no gate image configured"}, false},
		{"clone → not attempted", Inputs{Mode: ModeClone, Profile: ProfileLocal, Image: "img", Runtime: safeRT}, PathClone,
			[]string{"not attempted: mode=clone selects a fallback path"}, false},
		{"clone-sandbox → not attempted", Inputs{Mode: ModeCloneSandbox, Profile: ProfileLocal, Image: "img", Runtime: safeRT, Sandbox: sbOK}, PathCloneSandbox,
			[]string{"not attempted: mode=clone-sandbox selects a fallback path"}, false},
		{"clone/hosted refused → not attempted", Inputs{Mode: ModeClone, Profile: ProfileHosted, Image: "img", Runtime: safeRT}, PathRefused,
			[]string{"not attempted: mode=clone"}, false},
		{"unknown mode → not attempted", Inputs{Mode: "bogus", Profile: ProfileLocal, Image: "img", Runtime: safeRT}, PathRefused,
			[]string{`not attempted: unknown isolation mode "bogus"`}, false},
		{"auto/container → empty", Inputs{Mode: ModeAuto, Profile: ProfileLocal, Image: "img", Runtime: safeRT}, PathContainer, nil, true},
		{"container/container → empty", Inputs{Mode: ModeContainer, Profile: ProfileHosted, Image: "img", Runtime: safeRT}, PathContainer, nil, true},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			sel := Select(r.in)
			if sel.Path != r.path {
				t.Fatalf("path = %q, want %q", sel.Path, r.path)
			}
			if r.wantNil && sel.ContainerUnavailable != "" {
				t.Fatalf("container path carries ContainerUnavailable %q, want empty", sel.ContainerUnavailable)
			}
			if !r.wantNil && sel.ContainerUnavailable == "" {
				t.Fatalf("non-container path %q carries no ContainerUnavailable", sel.Path)
			}
			for _, w := range r.want {
				if !strings.Contains(sel.ContainerUnavailable, w) {
					t.Errorf("ContainerUnavailable %q lacks %q", sel.ContainerUnavailable, w)
				}
			}
		})
	}
}

// TestPathClass pins the container|fallback|refused mapping; an unknown path
// has no class.
func TestPathClass(t *testing.T) {
	for p, want := range map[Path]Class{
		PathContainer:    ClassContainer,
		PathCloneSandbox: ClassFallback,
		PathClone:        ClassFallback,
		PathRefused:      ClassRefused,
		Path("bogus"):    "",
	} {
		if got := p.Class(); got != want {
			t.Errorf("Path(%q).Class() = %q, want %q", p, got, want)
		}
	}
}

// TestSelect_ExistingInputsByteIdentical pins that the E51.3 / #2136 fields
// (all omitempty) leave a selection without an image request byte-identical:
// the three inputs behind the shared gate_isolation_evidence golden marshal
// to the bytes captured before the fields existed.
func TestSelect_ExistingInputsByteIdentical(t *testing.T) {
	const image = "ghcr.io/kuhlman-labs/fishhawk-gate:main"
	const podSock = "/run/user/1000/podman/podman.sock"
	rows := []struct {
		name string
		in   Inputs
		want string
	}{
		{"fallback", Inputs{Mode: ModeAuto, Profile: ProfileLocal,
			Runtime: Runtime{Kind: KindNone, Reason: "no container runtime on PATH (docker or podman)"},
			Sandbox: SandboxProbe{Reason: "unshare unavailable on darwin (ADR-063 gap)"}},
			`{"path":"clone","mode":"auto","profile":"local","runtime":{"kind":"none","safe":false,"reason":"no container runtime on PATH (docker or podman)","rootless":false,"runner_in_container":false,"endpoint":{"raw":"","scheme":"","local":false}},"sandbox":{"available":false,"reason":"unshare unavailable on darwin (ADR-063 gap)"},"reason":"mode=auto: container path unavailable (no safe container runtime (no container runtime on PATH (docker or podman)); no gate image configured (FISHHAWK_GATE_IMAGE is empty)); sandbox unavailable (unshare unavailable on darwin (ADR-063 gap)); falling back to clone","container_unavailable":"no safe container runtime (no container runtime on PATH (docker or podman)); no gate image configured (FISHHAWK_GATE_IMAGE is empty)"}`},
		{"refused", Inputs{Mode: ModeAuto, Profile: ProfileHosted, Image: image,
			Runtime: Runtime{Kind: KindDocker, Reason: "docker endpoint from DOCKER_HOST is not a local unix socket: scheme tcp",
				Endpoint: Endpoint{Raw: "tcp://10.0.0.5:2376", Scheme: "tcp"}},
			Sandbox: SandboxProbe{Available: true}},
			`{"path":"refused","mode":"auto","profile":"hosted","image":"ghcr.io/kuhlman-labs/fishhawk-gate:main","runtime":{"kind":"docker","safe":false,"reason":"docker endpoint from DOCKER_HOST is not a local unix socket: scheme tcp","rootless":false,"runner_in_container":false,"endpoint":{"raw":"tcp://10.0.0.5:2376","scheme":"tcp","local":false}},"sandbox":{"available":true},"reason":"profile=hosted refuses every non-container path and the container path is unavailable: docker is not a safe runtime (docker endpoint from DOCKER_HOST is not a local unix socket: scheme tcp)","container_unavailable":"docker is not a safe runtime (docker endpoint from DOCKER_HOST is not a local unix socket: scheme tcp)"}`},
		{"container", Inputs{Mode: ModeContainer, Profile: ProfileHosted, Image: image,
			Runtime: Runtime{Kind: KindPodman, Safe: true, Rootless: true, Version: "5.2.1",
				Reason:     "podman 5.2.1 over local unix socket " + podSock,
				Endpoint:   Endpoint{Raw: "unix://" + podSock, Scheme: "unix", Path: podSock, Local: true},
				SocketPath: podSock},
			Sandbox: SandboxProbe{Available: true}},
			`{"path":"container","mode":"container","profile":"hosted","image":"ghcr.io/kuhlman-labs/fishhawk-gate:main","runtime":{"kind":"podman","safe":true,"reason":"podman 5.2.1 over local unix socket /run/user/1000/podman/podman.sock","rootless":true,"runner_in_container":false,"version":"5.2.1","endpoint":{"raw":"unix:///run/user/1000/podman/podman.sock","scheme":"unix","path":"/run/user/1000/podman/podman.sock","local":true},"socket_path":"/run/user/1000/podman/podman.sock"},"sandbox":{"available":true},"reason":"mode=container: podman 5.2.1 over local unix socket /run/user/1000/podman/podman.sock"}`},
	}
	for _, r := range rows {
		raw, err := json.Marshal(Select(r.in))
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != r.want {
			t.Errorf("%s: selection bytes moved:\n got %s\nwant %s", r.name, raw, r.want)
		}
	}
}

// TestSelect_PolicyRefusalRefusesEveryMode: a non-empty PolicyRefusal refuses
// under every mode and profile, even where the container path or a fallback
// would otherwise be taken, and the selection keeps the declared image.
func TestSelect_PolicyRefusalRefusesEveryMode(t *testing.T) {
	const declared = "ghcr.io/org/gate:main"
	const refusal = "gate_container (stage) image ghcr.io/org/gate:main is not digest-pinned"
	for _, m := range modes {
		for _, p := range profiles {
			for _, build := range []bool{false, true} {
				in := Inputs{Mode: m, Profile: p, Image: declared, Build: build, ImageSource: ImageSourceStage, PolicyRefusal: refusal, Runtime: safeRT, Sandbox: sbOK}
				if build {
					in.Image = ""
				}
				sel := Select(in)
				if sel.Path != PathRefused || !sel.Refused() {
					t.Errorf("%s/%s/build=%v: path = %q, want refused", m, p, build, sel.Path)
				}
				if sel.Reason != "gate_container policy refused: "+refusal {
					t.Errorf("%s/%s: reason = %q", m, p, sel.Reason)
				}
				if sel.ContainerUnavailable != "not attempted: gate_container policy refused" {
					t.Errorf("%s/%s: container_unavailable = %q", m, p, sel.ContainerUnavailable)
				}
				if sel.Image != in.Image || sel.ImageSource != ImageSourceStage || sel.DeclaredUnhonored != "" {
					t.Errorf("%s/%s: image=%q source=%q unhonored=%q", m, p, sel.Image, sel.ImageSource, sel.DeclaredUnhonored)
				}
			}
		}
	}
}

// TestSelect_BuildSatisfiesContainerPath: a declared build with no image ref
// takes the container path on a safe runtime, and the "no gate image" part of
// containerMissing is suppressed when the runtime is what is missing.
func TestSelect_BuildSatisfiesContainerPath(t *testing.T) {
	for _, m := range []Mode{ModeAuto, ModeContainer} {
		for _, p := range profiles {
			sel := Select(Inputs{Mode: m, Profile: p, Build: true, ImageSource: ImageSourceWorkflow, Runtime: safeRT, Sandbox: sbNo})
			if sel.Path != PathContainer || sel.ContainerUnavailable != "" || !sel.Build || sel.Image != "" {
				t.Errorf("%s/%s: %+v, want the container path for a build", m, p, sel)
			}
		}
	}
	sel := Select(Inputs{Mode: ModeContainer, Profile: ProfileLocal, Build: true, ImageSource: ImageSourceWorkflow, Runtime: unsafeRT})
	if sel.Path != PathRefused || !strings.Contains(sel.ContainerUnavailable, "docker is not a safe runtime") || strings.Contains(sel.ContainerUnavailable, "no gate image configured") {
		t.Fatalf("build on an unsafe runtime: container_unavailable = %q", sel.ContainerUnavailable)
	}
}

// TestSelect_DeclaredUnhonoredMarker: a DECLARED source landing on a host
// fallback carries the marker naming what the container path lacked; an
// env-sourced fallback, a container path and every refusal carry none.
func TestSelect_DeclaredUnhonoredMarker(t *testing.T) {
	const declared = "ghcr.io/org/gate@sha256:" + "0000000000000000000000000000000000000000000000000000000000000000"
	rows := []struct {
		name string
		in   Inputs
		path Path
		want string
	}{
		{"local auto unsafe runtime", Inputs{Mode: ModeAuto, Profile: ProfileLocal, Image: declared, ImageSource: ImageSourceStage, Runtime: unsafeRT, Sandbox: sbNo}, PathClone,
			"gate_container declared (stage) but not honoured: docker is not a safe runtime (docker endpoint from DOCKER_HOST is not a local unix socket: scheme tcp); the gate ran on the host toolchain"},
		{"local mode=clone", Inputs{Mode: ModeClone, Profile: ProfileLocal, Image: declared, ImageSource: ImageSourceWorkflow, Runtime: safeRT}, PathClone,
			"gate_container declared (workflow) but not honoured: not attempted: mode=clone selects a fallback path; the gate ran on the host toolchain"},
		{"self-hosted clone-sandbox", Inputs{Mode: ModeCloneSandbox, Profile: ProfileSelfHosted, Build: true, ImageSource: ImageSourceStage, Runtime: safeRT, Sandbox: sbOK}, PathCloneSandbox,
			"gate_container declared (stage) but not honoured: not attempted: mode=clone-sandbox selects a fallback path; the gate ran on the host toolchain"},
		{"self-hosted auto no runtime build", Inputs{Mode: ModeAuto, Profile: ProfileSelfHosted, Build: true, ImageSource: ImageSourceWorkflow, Runtime: noRT, Sandbox: sbOK}, PathCloneSandbox,
			"gate_container declared (workflow) but not honoured: no safe container runtime (no container runtime on PATH (docker or podman)); the gate ran on the host toolchain"},
		{"env-sourced fallback", Inputs{Mode: ModeAuto, Profile: ProfileLocal, Image: "img:1", ImageSource: ImageSourceEnv, Runtime: unsafeRT, Sandbox: sbNo}, PathClone, ""},
		{"no source fallback", Inputs{Mode: ModeAuto, Profile: ProfileLocal, Runtime: noRT, Sandbox: sbNo}, PathClone, ""},
		{"hosted auto unsafe", Inputs{Mode: ModeAuto, Profile: ProfileHosted, Image: declared, ImageSource: ImageSourceStage, Runtime: unsafeRT, Sandbox: sbOK}, PathRefused, ""},
		{"local container unsafe", Inputs{Mode: ModeContainer, Profile: ProfileLocal, Image: declared, ImageSource: ImageSourceStage, Runtime: unsafeRT, Sandbox: sbOK}, PathRefused, ""},
		{"local clone-sandbox no sandbox", Inputs{Mode: ModeCloneSandbox, Profile: ProfileLocal, Image: declared, ImageSource: ImageSourceStage, Runtime: safeRT, Sandbox: sbNo}, PathRefused, ""},
		{"local auto container", Inputs{Mode: ModeAuto, Profile: ProfileLocal, Image: declared, ImageSource: ImageSourceStage, Runtime: safeRT}, PathContainer, ""},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			sel := Select(r.in)
			if sel.Path != r.path {
				t.Fatalf("path = %q, want %q (reason %q)", sel.Path, r.path, sel.Reason)
			}
			if sel.DeclaredUnhonored != r.want {
				t.Fatalf("declared_unhonored = %q, want %q", sel.DeclaredUnhonored, r.want)
			}
		})
	}
}

// TestSelect_EchoesImageRequest: the request fields are echoed onto the
// selection and marshal under their wire names; Select never fills the
// runner-owned ResolvedImage / DistinctImagesCount.
func TestSelect_EchoesImageRequest(t *testing.T) {
	const warn = "gate_container (workflow) image ghcr.io/org/gate:main is not digest-pinned"
	sel := Select(Inputs{Mode: ModeAuto, Profile: ProfileLocal, Image: "ghcr.io/org/gate:main", ImageSource: ImageSourceWorkflow, PolicyWarning: warn, Runtime: noRT, Sandbox: sbNo})
	if sel.ImageSource != ImageSourceWorkflow || sel.PolicyWarning != warn || sel.Build || sel.ResolvedImage != nil || sel.DistinctImagesCount != 0 {
		t.Fatalf("sel = %+v", sel)
	}
	sel.ResolvedImage = &ResolvedImage{Ref: "ghcr.io/org/gate@" + digA, Digest: digA, ImageID: "sha256:" + strings.Repeat("c", 64)}
	sel.DistinctImagesCount = 2
	raw, err := json.Marshal(sel)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"image_source":"workflow"`, `"policy_warning":"` + warn + `"`, `"declared_unhonored":"gate_container declared (workflow) but not honoured: `, `"resolved_image":{"ref":"ghcr.io/org/gate@` + digA + `","digest":"` + digA + `","image_id":"sha256:`, `"distinct_images_count":2`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("selection JSON %s lacks %s", raw, key)
		}
	}
	if strings.Contains(string(raw), `"build"`) || strings.Contains(string(raw), "build_dockerfile") {
		t.Errorf("zero build fields must be omitted: %s", raw)
	}
	b, err := json.Marshal(Select(Inputs{Mode: ModeAuto, Profile: ProfileLocal, Build: true, ImageSource: ImageSourceStage, Runtime: safeRT}))
	if err != nil || !strings.Contains(string(b), `"build":true`) || !strings.Contains(string(b), `"image_source":"stage"`) {
		t.Fatalf("build selection JSON = %s, %v", b, err)
	}
}

// TestResolvedImageIdentity pins the distinct-image key precedence.
func TestResolvedImageIdentity(t *testing.T) {
	rows := []struct {
		r    ResolvedImage
		want string
	}{
		{ResolvedImage{Ref: "r", Digest: "d", ContextDigest: "c", ImageID: "i"}, "d"},
		{ResolvedImage{Ref: "r", ContextDigest: "c", ImageID: "i"}, "c"},
		{ResolvedImage{Ref: "r", ImageID: "i"}, "i"},
		{ResolvedImage{Ref: "r"}, "r"},
		{ResolvedImage{}, ""},
	}
	for _, r := range rows {
		if got := r.r.Identity(); got != r.want {
			t.Errorf("%+v.Identity() = %q, want %q", r.r, got, r.want)
		}
	}
}
