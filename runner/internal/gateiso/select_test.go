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
