package gateiso

import (
	"fmt"
	"strings"
)

// Mode is the operator-requested isolation mode (FISHHAWK_GATE_ISOLATION).
type Mode string

// Isolation modes. ModeAuto is the default: container when a safe runtime and
// an image (or a declared build) are present, else the strongest available
// fallback.
const (
	ModeAuto         Mode = "auto"
	ModeContainer    Mode = "container"
	ModeCloneSandbox Mode = "clone-sandbox"
	ModeClone        Mode = "clone"
)

var modes = []Mode{ModeAuto, ModeContainer, ModeCloneSandbox, ModeClone}

// Profile is the runner-declared deployment profile
// (FISHHAWK_DEPLOYMENT_PROFILE). Default local. The profile is DECLARED, not
// detected: a hosted deployment that forgets to set it gets fallback rather
// than refusal — a documented residual (#2138 owns the operator docs).
type Profile string

// Deployment profiles. ProfileHosted refuses every non-container path.
const (
	ProfileLocal      Profile = "local"
	ProfileSelfHosted Profile = "self-hosted"
	ProfileHosted     Profile = "hosted"
)

var profiles = []Profile{ProfileLocal, ProfileSelfHosted, ProfileHosted}

// Path is the execution path Select decided on.
type Path string

// Execution paths. PathRefused means no acceptable path exists and the gate
// must NOT execute (the runner reports category C).
const (
	PathContainer    Path = "container"
	PathCloneSandbox Path = "clone-sandbox"
	PathClone        Path = "clone"
	PathRefused      Path = "refused"
)

// Class is the coarse isolation class a Path belongs to (#2135): the
// container|fallback|refused vocabulary the gate evidence and the gate view
// carry beside the precise Path.
type Class string

// Isolation classes. ClassFallback covers both host paths (clone-sandbox and
// clone): the gate ran, but outside a container.
const (
	ClassContainer Class = "container"
	ClassFallback  Class = "fallback"
	ClassRefused   Class = "refused"
)

// Class maps the path to its isolation class; an unknown path maps to "".
func (p Path) Class() Class {
	switch p {
	case PathContainer:
		return ClassContainer
	case PathCloneSandbox, PathClone:
		return ClassFallback
	case PathRefused:
		return ClassRefused
	}
	return ""
}

// ParseMode parses an isolation mode; empty selects ModeAuto. The error names
// the valid values.
func ParseMode(s string) (Mode, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return ModeAuto, nil
	}
	for _, m := range modes {
		if Mode(s) == m {
			return m, nil
		}
	}
	return "", fmt.Errorf("invalid gate isolation mode %q (valid: %s)", s, joinModes())
}

// ParseProfile parses a deployment profile; empty selects ProfileLocal. The
// error names the valid values.
func ParseProfile(s string) (Profile, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return ProfileLocal, nil
	}
	for _, p := range profiles {
		if Profile(s) == p {
			return p, nil
		}
	}
	return "", fmt.Errorf("invalid deployment profile %q (valid: %s)", s, joinProfiles())
}

func joinModes() string {
	out := make([]string, len(modes))
	for i, m := range modes {
		out[i] = string(m)
	}
	return strings.Join(out, ", ")
}

func joinProfiles() string {
	out := make([]string, len(profiles))
	for i, p := range profiles {
		out[i] = string(p)
	}
	return strings.Join(out, ", ")
}

// SandboxProbe is the availability verdict of the Linux no-network sandbox
// (unshare -rn); non-Linux hosts report Available=false with the ADR-063 gap
// reason.
type SandboxProbe struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// Inputs is everything Select reads. Select is PURE over these: no probe, no
// filesystem, no environment.
type Inputs struct {
	Mode    Mode
	Profile Profile
	// Image is the gate image the request resolved to: a declared
	// gate_container image ref, or FISHHAWK_GATE_IMAGE. "" with Build false
	// means the container path is unavailable regardless of the runtime.
	Image   string
	Runtime Runtime
	Sandbox SandboxProbe
	// Build reports a declared in-repo Dockerfile build (E51.3 / #2136): it
	// satisfies the container path's image requirement, the runner building
	// the image before the first gate exec.
	Build bool
	// ImageSource is where the image request came from (ImageSourceStage,
	// ImageSourceWorkflow, ImageSourceEnv, or "" for none).
	ImageSource string
	// PolicyRefusal is EvaluateImagePolicy's refusal for the request; non-empty
	// refuses the gate under EVERY mode and never substitutes another image.
	PolicyRefusal string
	// PolicyWarning is EvaluateImagePolicy's allowed-but-weak finding, echoed
	// onto the selection.
	PolicyWarning string
}

// ResolvedImage is what a declared gate_container resolved to on the host —
// filled by the runner after its pull or build, never by Select. It records
// the image of the FINAL DECIDING gate of the stage.
type ResolvedImage struct {
	// Ref is the reference the gate ran by: name@digest for a pulled image,
	// the content-addressed build tag for a built image.
	Ref string `json:"ref,omitempty"`
	// Digest is the registry content digest (sha256:<hex>) of a pulled image.
	Digest string `json:"digest,omitempty"`
	// ImageID is the runtime's local image id: opaque, recorded, never
	// compared across hosts (a containerd image store reports an index
	// digest there).
	ImageID string `json:"image_id,omitempty"`
	// BuildDockerfile / BuildContext are the declared repo-relative build
	// paths.
	BuildDockerfile string `json:"build_dockerfile,omitempty"`
	BuildContext    string `json:"build_context,omitempty"`
	// ContextDigest is the build's content digest (the build tag's key).
	ContextDigest string `json:"context_digest,omitempty"`
}

// Identity is the key a stage counts distinct gate images by: the registry
// digest when known, else the build content digest, else the local image id,
// else the ref.
func (r ResolvedImage) Identity() string {
	for _, k := range []string{r.Digest, r.ContextDigest, r.ImageID, r.Ref} {
		if k != "" {
			return k
		}
	}
	return ""
}

// Selection is the recorded decision; the runner flattens it into the #2135
// gate_isolation evidence member.
type Selection struct {
	Path    Path         `json:"path"`
	Mode    Mode         `json:"mode"`
	Profile Profile      `json:"profile"`
	Image   string       `json:"image,omitempty"`
	Runtime Runtime      `json:"runtime"`
	Sandbox SandboxProbe `json:"sandbox"`
	Reason  string       `json:"reason"`
	// ContainerUnavailable names why the container path was not taken, on
	// EVERY non-container outcome (#2135): what the container path lacked
	// (containerMissing) when it was considered, or "not attempted: ..." when
	// the mode never considers it. Empty on the container path.
	ContainerUnavailable string `json:"container_unavailable,omitempty"`
	// ImageSource, Build and PolicyWarning echo the image request (E51.3 /
	// #2136); all omitempty, so a selection without a request is
	// byte-identical to its pre-#2136 form.
	ImageSource   string `json:"image_source,omitempty"`
	Build         bool   `json:"build,omitempty"`
	PolicyWarning string `json:"policy_warning,omitempty"`
	// DeclaredUnhonored is set when a DECLARED gate_container (stage or
	// workflow) was not honoured because the gate ran on a host fallback path
	// (clone-sandbox or clone); it names what the container path lacked.
	DeclaredUnhonored string `json:"declared_unhonored,omitempty"`
	// ResolvedImage is the image the final deciding gate ran in, filled by the
	// runner after resolution; nil from Select.
	ResolvedImage *ResolvedImage `json:"resolved_image,omitempty"`
	// DistinctImagesCount is the number of distinct resolved images the
	// stage's gates ran in, set by the runner only when it exceeds one.
	DistinctImagesCount int `json:"distinct_images_count,omitempty"`
}

// Refused reports whether the selection forbids executing the gate.
func (s Selection) Refused() bool { return s.Path == PathRefused }

// Select decides the execution path from mode × profile × image × runtime ×
// sandbox, then marks a declared gate_container the path did not honour:
//
//   - a non-empty PolicyRefusal → refused under every mode, the container
//     path not attempted.
//   - container requires Runtime.Safe && (Image != "" || Build).
//   - mode=container → container, else refused.
//   - mode=auto → container when possible; otherwise refused under hosted
//     (naming what is missing), else clone-sandbox when available, else clone.
//   - mode=clone-sandbox → refused under hosted; else clone-sandbox when
//     available, else refused.
//   - mode=clone → refused under hosted; else clone.
//
// Hosted never executes an untrusted gate outside a container: the fallback
// paths share the host's filesystem and daemon sockets with the runner, so a
// declared image there is refused, never run on the host.
//
// A declared source (stage or workflow) landing on clone-sandbox or clone
// carries DeclaredUnhonored; an env-sourced fallback does not.
func Select(in Inputs) Selection {
	sel := selectPath(in)
	sel.ImageSource, sel.Build, sel.PolicyWarning = in.ImageSource, in.Build, in.PolicyWarning
	if DeclaredSource(in.ImageSource) && sel.Path.Class() == ClassFallback {
		sel.DeclaredUnhonored = fmt.Sprintf("gate_container declared (%s) but not honoured: %s; the gate ran on the host toolchain", in.ImageSource, sel.ContainerUnavailable)
	}
	return sel
}

// selectPath is Select's mode × profile decision.
func selectPath(in Inputs) Selection {
	sel := Selection{Path: PathRefused, Mode: in.Mode, Profile: in.Profile, Image: in.Image, Runtime: in.Runtime, Sandbox: in.Sandbox}
	if in.PolicyRefusal != "" {
		sel.ContainerUnavailable = "not attempted: gate_container policy refused"
		sel.Reason = "gate_container policy refused: " + in.PolicyRefusal
		return sel
	}
	containerOK := in.Runtime.Safe && (in.Image != "" || in.Build)
	missing := containerMissing(in)
	switch in.Mode {
	case ModeContainer:
		if containerOK {
			sel.Path = PathContainer
			sel.Reason = "mode=container: " + in.Runtime.Reason
			return sel
		}
		sel.ContainerUnavailable = missing
		sel.Reason = "mode=container but the container path is unavailable: " + missing
		return sel
	case ModeAuto:
		if containerOK {
			sel.Path = PathContainer
			sel.Reason = "mode=auto: container path available (" + in.Runtime.Reason + ")"
			return sel
		}
		sel.ContainerUnavailable = missing
		if in.Profile == ProfileHosted {
			sel.Reason = "profile=hosted refuses every non-container path and the container path is unavailable: " + missing
			return sel
		}
		if in.Sandbox.Available {
			sel.Path = PathCloneSandbox
			sel.Reason = "mode=auto: container path unavailable (" + missing + "); falling back to clone-sandbox"
			return sel
		}
		sel.Path = PathClone
		sel.Reason = "mode=auto: container path unavailable (" + missing + "); sandbox unavailable (" + in.Sandbox.Reason + "); falling back to clone"
		return sel
	case ModeCloneSandbox:
		sel.ContainerUnavailable = notAttempted(in.Mode)
		if in.Profile == ProfileHosted {
			sel.Reason = "profile=hosted refuses mode=clone-sandbox: only the container path is permitted"
			return sel
		}
		if in.Sandbox.Available {
			sel.Path = PathCloneSandbox
			sel.Reason = "mode=clone-sandbox: sandbox available"
			return sel
		}
		sel.Reason = "mode=clone-sandbox but the sandbox is unavailable: " + in.Sandbox.Reason
		return sel
	case ModeClone:
		sel.ContainerUnavailable = notAttempted(in.Mode)
		if in.Profile == ProfileHosted {
			sel.Reason = "profile=hosted refuses mode=clone: only the container path is permitted"
			return sel
		}
		sel.Path = PathClone
		sel.Reason = "mode=clone: host exec in a throwaway clone"
		return sel
	}
	sel.ContainerUnavailable = fmt.Sprintf("not attempted: unknown isolation mode %q", in.Mode)
	sel.Reason = fmt.Sprintf("unknown isolation mode %q", in.Mode)
	return sel
}

// notAttempted is ContainerUnavailable for an explicit fallback mode, which
// never considers the container path.
func notAttempted(m Mode) string {
	return fmt.Sprintf("not attempted: mode=%s selects a fallback path", m)
}

// containerMissing names what the container path lacks.
func containerMissing(in Inputs) string {
	var parts []string
	if !in.Runtime.Safe {
		if in.Runtime.Kind == KindNone || in.Runtime.Kind == "" {
			parts = append(parts, "no safe container runtime ("+in.Runtime.Reason+")")
		} else {
			parts = append(parts, string(in.Runtime.Kind)+" is not a safe runtime ("+in.Runtime.Reason+")")
		}
	}
	if in.Image == "" && !in.Build {
		parts = append(parts, "no gate image configured (FISHHAWK_GATE_IMAGE is empty)")
	}
	if len(parts) == 0 {
		return "container path available"
	}
	return strings.Join(parts, "; ")
}

// ProfileForbidsFallback is the STARTUP check: a hosted profile combined with
// an explicit fallback mode is a configuration error the runner rejects
// before contacting the backend, rather than a per-gate refusal.
func ProfileForbidsFallback(p Profile, m Mode) error {
	if p != ProfileHosted {
		return nil
	}
	switch m {
	case ModeClone, ModeCloneSandbox:
		return fmt.Errorf("deployment profile %s forbids gate isolation mode %s: only auto or container are permitted", p, m)
	}
	return nil
}
