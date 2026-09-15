package gateiso

import (
	"fmt"
	"strings"
)

// Mode is the operator-requested isolation mode (FISHHAWK_GATE_ISOLATION).
type Mode string

// Isolation modes. ModeAuto is the default: container when a safe runtime and
// an image are present, else the strongest available fallback.
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
	// Image is the configured gate image (FISHHAWK_GATE_IMAGE); "" means the
	// container path is unavailable regardless of the runtime.
	Image   string
	Runtime Runtime
	Sandbox SandboxProbe
}

// Selection is the recorded decision (the struct #2135 evidence carries).
type Selection struct {
	Path    Path         `json:"path"`
	Mode    Mode         `json:"mode"`
	Profile Profile      `json:"profile"`
	Image   string       `json:"image,omitempty"`
	Runtime Runtime      `json:"runtime"`
	Sandbox SandboxProbe `json:"sandbox"`
	Reason  string       `json:"reason"`
}

// Refused reports whether the selection forbids executing the gate.
func (s Selection) Refused() bool { return s.Path == PathRefused }

// Select decides the execution path from mode × profile × image × runtime ×
// sandbox:
//
//   - container requires Runtime.Safe && Image != "".
//   - mode=container → container, else refused.
//   - mode=auto → container when possible; otherwise refused under hosted
//     (naming what is missing), else clone-sandbox when available, else clone.
//   - mode=clone-sandbox → refused under hosted; else clone-sandbox when
//     available, else refused.
//   - mode=clone → refused under hosted; else clone.
//
// Hosted never executes an untrusted gate outside a container: the fallback
// paths share the host's filesystem and daemon sockets with the runner.
func Select(in Inputs) Selection {
	sel := Selection{Path: PathRefused, Mode: in.Mode, Profile: in.Profile, Image: in.Image, Runtime: in.Runtime, Sandbox: in.Sandbox}
	containerOK := in.Runtime.Safe && in.Image != ""
	missing := containerMissing(in)
	switch in.Mode {
	case ModeContainer:
		if containerOK {
			sel.Path = PathContainer
			sel.Reason = "mode=container: " + in.Runtime.Reason
			return sel
		}
		sel.Reason = "mode=container but the container path is unavailable: " + missing
		return sel
	case ModeAuto:
		if containerOK {
			sel.Path = PathContainer
			sel.Reason = "mode=auto: container path available (" + in.Runtime.Reason + ")"
			return sel
		}
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
		if in.Profile == ProfileHosted {
			sel.Reason = "profile=hosted refuses mode=clone: only the container path is permitted"
			return sel
		}
		sel.Path = PathClone
		sel.Reason = "mode=clone: host exec in a throwaway clone"
		return sel
	}
	sel.Reason = fmt.Sprintf("unknown isolation mode %q", in.Mode)
	return sel
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
	if in.Image == "" {
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
