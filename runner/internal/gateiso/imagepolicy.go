package gateiso

import (
	"fmt"
	"regexp"
	"strings"
)

// This file is the PURE image policy for a project-declared gate container
// (E51.3 / #2136): the workflow-v2 `gate_container` block names either an
// image reference or an in-repo Dockerfile build, and the runner decides here
// — before any pull, build or gate exec — whether the deployment profile and
// the operator's allowlist (FISHHAWK_GATE_IMAGE_ALLOWLIST) and build posture
// (FISHHAWK_GATE_BUILD) permit it. Nothing here touches the host.

// Image sources: where the gate image request came from. A stage-level
// gate_container beats the workflow-level block, which beats the operator's
// FISHHAWK_GATE_IMAGE; "" means no request at all (today's fallback).
const (
	ImageSourceStage    = "stage"
	ImageSourceWorkflow = "workflow"
	ImageSourceEnv      = "env"
)

// DeclaredSource reports whether source names a project-declared
// gate_container (stage or workflow), as opposed to the operator's env image
// or no request.
func DeclaredSource(source string) bool {
	return source == ImageSourceStage || source == ImageSourceWorkflow
}

// ImageRef is a normalized docker-style image reference. Registry and Path
// are always populated; Tag and Digest are each optional. A reference with
// neither is tag-only in effect (the runtime pulls :latest).
type ImageRef struct {
	// Registry is the lowercase registry host, with its port when one was
	// written ("ghcr.io", "localhost:5000"); "docker.io" when the reference
	// names none (index.docker.io normalizes to docker.io).
	Registry string
	// Path is the repository path ("org/gate"); a single-component Docker Hub
	// path gains "library/" ("library/alpine").
	Path string
	// Tag is the tag without its colon ("" when absent).
	Tag string
	// Digest is "sha256:<64 lowercase hex>" ("" when absent).
	Digest string
}

// Name is the normalized repository name: Registry + "/" + Path.
func (r ImageRef) Name() string { return r.Registry + "/" + r.Path }

// String renders the normalized reference: Name, then ":tag", then "@digest".
func (r ImageRef) String() string {
	s := r.Name()
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

// Pinned reports whether the reference carries a content digest, so what runs
// cannot change under a retag.
func (r ImageRef) Pinned() bool { return r.Digest != "" }

const (
	// defaultRegistry is the registry a reference without one resolves to.
	defaultRegistry = "docker.io"
	// legacyDefaultRegistry normalizes to defaultRegistry, as the docker CLI
	// does.
	legacyDefaultRegistry = "index.docker.io"
	// maxImageRefLen bounds a whole reference (the workflow-v2 schema's cap).
	maxImageRefLen = 512
	// maxImageNameLen bounds the normalized name (the docker reference cap).
	maxImageNameLen = 255
)

var (
	// hostRE is a registry host: dot-separated domain components, then an
	// optional numeric port. IPv6 literals are not accepted.
	hostRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)*(?::[0-9]+)?$`)
	// pathComponentRE is one lowercase repository path component.
	pathComponentRE = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	tagRE           = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestRE        = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// isHostComponent reports whether the first component of a name is a registry
// host rather than a repository path component: it contains a '.' or a ':'
// (a port), or is exactly "localhost" — the docker CLI's rule.
func isHostComponent(s string) bool {
	return strings.ContainsAny(s, ".:") || s == "localhost"
}

// normalizeHost validates and lowercases a registry host.
func normalizeHost(h string) (string, error) {
	if !hostRE.MatchString(h) {
		return "", fmt.Errorf("invalid registry host %q", h)
	}
	h = strings.ToLower(h)
	if h == legacyDefaultRegistry {
		h = defaultRegistry
	}
	return h, nil
}

// validatePath checks every repository path component.
func validatePath(p string) error {
	if p == "" {
		return fmt.Errorf("empty repository path")
	}
	for _, c := range strings.Split(p, "/") {
		if !pathComponentRE.MatchString(c) {
			return fmt.Errorf("invalid repository path component %q (lowercase letters, digits and . _ - separators only)", c)
		}
	}
	return nil
}

// ParseImageRef parses and normalizes a docker-style image reference:
// [registry/]path[:tag][@sha256:<64 hex>]. The first path component is the
// registry when it contains '.' or ':' or is "localhost", else the registry
// is docker.io and a single-component path gains "library/". Repository path
// components must be lowercase; the only accepted digest algorithm is sha256
// with 64 lowercase hex characters.
func ParseImageRef(s string) (ImageRef, error) {
	if s == "" {
		return ImageRef{}, fmt.Errorf("empty image reference")
	}
	if len(s) > maxImageRefLen {
		return ImageRef{}, fmt.Errorf("image reference is %d bytes, over the %d-byte limit", len(s), maxImageRefLen)
	}
	var ref ImageRef
	rest := s
	if i := strings.Index(rest, "@"); i >= 0 {
		ref.Digest = rest[i+1:]
		rest = rest[:i]
		if !digestRE.MatchString(ref.Digest) {
			return ImageRef{}, fmt.Errorf("invalid digest %q in %q (want sha256:<64 lowercase hex>)", ref.Digest, s)
		}
	}
	if j := strings.LastIndex(rest, ":"); j > strings.LastIndex(rest, "/") {
		ref.Tag = rest[j+1:]
		rest = rest[:j]
		if !tagRE.MatchString(ref.Tag) {
			return ImageRef{}, fmt.Errorf("invalid tag %q in %q", ref.Tag, s)
		}
	}
	ref.Registry, ref.Path = defaultRegistry, rest
	if i := strings.Index(rest, "/"); i >= 0 && isHostComponent(rest[:i]) {
		host, err := normalizeHost(rest[:i])
		if err != nil {
			return ImageRef{}, fmt.Errorf("%w in %q", err, s)
		}
		ref.Registry, ref.Path = host, rest[i+1:]
	}
	if err := validatePath(ref.Path); err != nil {
		return ImageRef{}, fmt.Errorf("%w in %q", err, s)
	}
	if ref.Registry == defaultRegistry && !strings.Contains(ref.Path, "/") {
		ref.Path = "library/" + ref.Path
	}
	if n := len(ref.Name()); n > maxImageNameLen {
		return ImageRef{}, fmt.Errorf("image name %q is %d bytes, over the %d-byte limit", ref.Name(), n, maxImageNameLen)
	}
	return ref, nil
}

// AllowlistKind is the kind of one allowlist entry.
type AllowlistKind string

// Allowlist entry kinds — exactly four.
const (
	// AllowRegistry permits every image on one registry host (host and port
	// compared exactly: "localhost" does not permit "localhost:5000").
	AllowRegistry AllowlistKind = "registry"
	// AllowNamespace permits every repository under a path prefix, matched on
	// whole path components ("ghcr.io/org/" never permits
	// "ghcr.io/organization/x").
	AllowNamespace AllowlistKind = "namespace"
	// AllowRepository permits any tag or digest of exactly one repository.
	AllowRepository AllowlistKind = "repository"
	// AllowDigest permits exactly one repository at exactly one digest; a
	// tag-only reference to that repository is NOT permitted.
	AllowDigest AllowlistKind = "digest"
)

// AllowlistEntry is one parsed FISHHAWK_GATE_IMAGE_ALLOWLIST entry.
type AllowlistEntry struct {
	Kind     AllowlistKind
	Registry string
	// Path is the namespace prefix (AllowNamespace, no trailing slash) or the
	// repository path (AllowRepository, AllowDigest); "" for AllowRegistry.
	Path   string
	Digest string
	// Raw is the entry as the operator wrote it.
	Raw string
}

// Allowlist is the parsed operator image allowlist. An empty allowlist
// constrains nothing in local/self-hosted and permits no declared image or
// build under the hosted profile.
type Allowlist []AllowlistEntry

// Empty reports whether no entry is configured.
func (a Allowlist) Empty() bool { return len(a) == 0 }

// Permits reports whether any entry permits ref. Registries compare exactly
// after normalization; repository paths match on whole components.
func (a Allowlist) Permits(ref ImageRef) bool {
	for _, e := range a {
		if e.permits(ref) {
			return true
		}
	}
	return false
}

func (e AllowlistEntry) permits(ref ImageRef) bool {
	if ref.Registry != e.Registry {
		return false
	}
	switch e.Kind {
	case AllowRegistry:
		return true
	case AllowNamespace:
		return pathWithin(ref.Path, e.Path)
	case AllowRepository:
		return ref.Path == e.Path
	case AllowDigest:
		return ref.Path == e.Path && ref.Digest != "" && ref.Digest == e.Digest
	}
	return false
}

// pathWithin reports whether path lies strictly under the namespace prefix ns,
// on a component boundary.
func pathWithin(path, ns string) bool {
	return strings.HasPrefix(path, ns+"/")
}

// ParseAllowlist parses FISHHAWK_GATE_IMAGE_ALLOWLIST: entries separated by
// commas and/or whitespace, each exactly one of
//
//   - a registry host: "ghcr.io", "registry:5000", "localhost", "localhost:5000";
//   - a namespace prefix with a trailing slash: "ghcr.io/org/";
//   - a repository: "ghcr.io/org/gate", "docker.io/library/alpine";
//   - an exact digest: "ghcr.io/org/gate@sha256:<64 hex>".
//
// Every non-host entry must name its registry explicitly. It REFUSES, naming
// the entry: a single-component bare name ("alpine" — ambiguous between a
// registry host and docker.io/library/alpine), a tag-bearing entry (a tag is
// mutable), any wildcard, and anything unparsable. An empty value is an empty
// allowlist.
func ParseAllowlist(s string) (Allowlist, error) {
	var out Allowlist
	for _, raw := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r' }) {
		e, err := parseAllowlistEntry(raw)
		if err != nil {
			return nil, fmt.Errorf("allowlist entry %q: %w", raw, err)
		}
		out = append(out, e)
	}
	return out, nil
}

func parseAllowlistEntry(raw string) (AllowlistEntry, error) {
	if strings.ContainsAny(raw, "*?[]{}") {
		return AllowlistEntry{}, fmt.Errorf("wildcards are not supported: write a registry host, a namespace ending in '/', a repository, or a repository@sha256 digest")
	}
	if !strings.ContainsAny(raw, "/@") {
		if !isHostComponent(raw) {
			return AllowlistEntry{}, fmt.Errorf("a bare name is ambiguous: write the repository with its registry (e.g. docker.io/library/%s) or a registry host with a dot or a port", raw)
		}
		host, err := normalizeHost(raw)
		if err != nil {
			return AllowlistEntry{}, err
		}
		return AllowlistEntry{Kind: AllowRegistry, Registry: host, Raw: raw}, nil
	}
	first, _, _ := strings.Cut(raw, "/")
	if !strings.Contains(raw, "/") || !isHostComponent(first) {
		return AllowlistEntry{}, fmt.Errorf("the entry must name its registry explicitly (e.g. docker.io/library/alpine, ghcr.io/org/gate)")
	}
	if strings.HasSuffix(raw, "/") {
		prefix := strings.TrimSuffix(raw, "/")
		hostPart, ns, ok := strings.Cut(prefix, "/")
		if !ok || ns == "" {
			return AllowlistEntry{}, fmt.Errorf("a namespace entry names a registry and a namespace (e.g. ghcr.io/org/); for a whole registry write the host without a trailing slash")
		}
		if strings.ContainsAny(ns, ":@") {
			return AllowlistEntry{}, fmt.Errorf("a namespace entry carries no tag or digest")
		}
		host, err := normalizeHost(hostPart)
		if err != nil {
			return AllowlistEntry{}, err
		}
		if err := validatePath(ns); err != nil {
			return AllowlistEntry{}, err
		}
		return AllowlistEntry{Kind: AllowNamespace, Registry: host, Path: ns, Raw: raw}, nil
	}
	ref, err := ParseImageRef(raw)
	if err != nil {
		return AllowlistEntry{}, err
	}
	if ref.Tag != "" {
		return AllowlistEntry{}, fmt.Errorf("a tag is mutable and cannot be allowlisted: write the repository (%s) or pin a digest (%s@sha256:<digest>)", ref.Name(), ref.Name())
	}
	if ref.Digest != "" {
		return AllowlistEntry{Kind: AllowDigest, Registry: ref.Registry, Path: ref.Path, Digest: ref.Digest, Raw: raw}, nil
	}
	return AllowlistEntry{Kind: AllowRepository, Registry: ref.Registry, Path: ref.Path, Raw: raw}, nil
}

// Build policy values (FISHHAWK_GATE_BUILD).
const (
	BuildPolicyAllow = "allow"
	BuildPolicyDeny  = "deny"
)

// ParseBuildPolicy parses FISHHAWK_GATE_BUILD into whether an in-repo gate
// image build is allowed: allow | deny, or empty for the profile default —
// denied under hosted, allowed under local and self-hosted (and denied under
// any other profile, failing closed).
func ParseBuildPolicy(value string, profile Profile) (bool, error) {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "":
		return !strictProfile(profile), nil
	case BuildPolicyAllow:
		return true, nil
	case BuildPolicyDeny:
		return false, nil
	}
	return false, fmt.Errorf("invalid gate build policy %q (valid: %s, %s, or empty for the profile default)", value, BuildPolicyAllow, BuildPolicyDeny)
}

// strictProfile reports whether a profile gets the hosted posture. Local and
// self-hosted are relaxed; hosted and any unrecognised profile are strict.
func strictProfile(p Profile) bool {
	return p != ProfileLocal && p != ProfileSelfHosted
}

// ImageRequest is the gate image the runner was asked to use: Source is
// ImageSourceStage/Workflow (a declared gate_container), ImageSourceEnv
// (FISHHAWK_GATE_IMAGE) or "" (none). A declared request names Image OR the
// Dockerfile+Context pair, never both.
type ImageRequest struct {
	Source     string
	Image      string
	Dockerfile string
	Context    string
}

// ImageDecision is EvaluateImagePolicy's verdict.
type ImageDecision struct {
	// Allowed is false exactly when Refusal is set.
	Allowed bool
	// Refusal names why the request is refused; the runner refuses the gate
	// (category C) with it and never substitutes another image.
	Refusal string
	// Warning is an allowed-but-weak finding (a tag-only declared reference
	// outside hosted) the runner logs and records.
	Warning string
	// Ref is the parsed declared image (zero for env, build, or no request).
	Ref ImageRef
	// Build reports a declared in-repo Dockerfile build.
	Build bool
	// BasesMustPass requires every build base (FROM, COPY --from=<image>,
	// RUN --mount from=<image>) to pass the allowlist.
	BasesMustPass bool
	// RequirePinnedBases additionally requires every build base to be
	// digest-pinned (hosted).
	RequirePinnedBases bool
}

func refuse(format string, args ...any) ImageDecision {
	return ImageDecision{Refusal: fmt.Sprintf(format, args...)}
}

// maxBuildPathLen bounds a declared dockerfile/context path (the schema cap).
const maxBuildPathLen = 1024

// buildPathRE mirrors the workflow-v2 schema's repo-relative path pattern.
var buildPathRE = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]*$`)

// checkRepoRelative mirrors the schema's dockerfile/context rule on the wire
// value: repo-relative, no absolute path, no ".." segment.
func checkRepoRelative(field, p string) error {
	if len(p) > maxBuildPathLen {
		return fmt.Errorf("%s is %d bytes, over the %d-byte limit", field, len(p), maxBuildPathLen)
	}
	if !buildPathRE.MatchString(p) {
		return fmt.Errorf("%s %q is not a repo-relative path", field, p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("%s %q escapes the repository through a '..' segment", field, p)
		}
	}
	return nil
}

// EvaluateImagePolicy decides whether profile, the operator allowlist and the
// build posture permit req. Every refusal holds under every mode; the caller
// never falls back to FISHHAWK_GATE_IMAGE on a refusal.
//
//   - No request: allowed, nothing to check.
//   - Env (FISHHAWK_GATE_IMAGE): operator-chosen, allowed unchecked.
//   - Declared image: unparsable → refused; allowlist configured and not
//     permitting → refused (every profile); hosted with an empty allowlist →
//     refused; tag-only → refused under hosted, allowed with a Warning
//     elsewhere.
//   - Declared build: builds disabled → refused; hosted with an empty
//     allowlist → refused; allowlist configured → allowed with BasesMustPass
//     (plus RequirePinnedBases under hosted); else allowed, bases
//     unconstrained.
//
// A declared request naming both sources, only half of the Dockerfile+Context
// pair, or a non-repo-relative path is refused.
func EvaluateImagePolicy(profile Profile, req ImageRequest, allow Allowlist, buildAllowed bool) ImageDecision {
	hasImage := req.Image != ""
	hasBuild := req.Dockerfile != "" || req.Context != ""
	switch {
	case req.Source == "" && !hasImage && !hasBuild:
		return ImageDecision{Allowed: true}
	case req.Source == ImageSourceEnv:
		if hasBuild {
			return refuse("gate image request from %s carries a dockerfile/context; only a declared gate_container can build", ImageSourceEnv)
		}
		return ImageDecision{Allowed: true}
	case !DeclaredSource(req.Source):
		return refuse("gate image request has unknown source %q", req.Source)
	}
	strict := strictProfile(profile)
	label := "gate_container (" + req.Source + ")"
	switch {
	case hasImage && hasBuild:
		return refuse("%s declares both image and dockerfile/context; exactly one source is allowed", label)
	case hasImage:
		ref, err := ParseImageRef(req.Image)
		if err != nil {
			return refuse("%s image is not a valid image reference: %v", label, err)
		}
		if !allow.Empty() && !allow.Permits(ref) {
			return refuse("%s image %s is not permitted by the operator image allowlist (FISHHAWK_GATE_IMAGE_ALLOWLIST)", label, ref)
		}
		if strict && allow.Empty() {
			return refuse("%s image %s refused: profile=%s permits a declared gate image only through an operator image allowlist (FISHHAWK_GATE_IMAGE_ALLOWLIST is empty)", label, ref, profile)
		}
		if !ref.Pinned() {
			if strict {
				return refuse("%s image %s is not digest-pinned: profile=%s requires %s@sha256:<digest>", label, ref, profile, ref.Name())
			}
			return ImageDecision{Allowed: true, Ref: ref, Warning: fmt.Sprintf("%s image %s is not digest-pinned: it is pulled on every gate and run by the digest the pull resolved; pin %s@sha256:<digest> to make the gate image reproducible", label, ref, ref.Name())}
		}
		return ImageDecision{Allowed: true, Ref: ref}
	case req.Dockerfile == "" || req.Context == "":
		return refuse("%s must declare dockerfile and context together", label)
	}
	if err := checkRepoRelative("dockerfile", req.Dockerfile); err != nil {
		return refuse("%s %v", label, err)
	}
	if err := checkRepoRelative("context", req.Context); err != nil {
		return refuse("%s %v", label, err)
	}
	if !buildAllowed {
		return refuse("%s refused: in-repo gate image builds are disabled under profile %s (FISHHAWK_GATE_BUILD)", label, profile)
	}
	if strict && allow.Empty() {
		return refuse("%s refused: profile=%s permits an in-repo gate image build only with an operator image allowlist for its bases (FISHHAWK_GATE_IMAGE_ALLOWLIST is empty)", label, profile)
	}
	if !allow.Empty() {
		return ImageDecision{Allowed: true, Build: true, BasesMustPass: true, RequirePinnedBases: strict}
	}
	return ImageDecision{Allowed: true, Build: true}
}
