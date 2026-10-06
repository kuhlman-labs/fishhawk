package gateiso

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// This file is the STATIC Dockerfile guard for a declared in-repo gate image
// build (E51.3 / #2136). `docker build --network=none` confines only RUN
// steps: the builder itself fetches base images, remote ADD sources and a
// `# syntax=` frontend image outside that namespace, and a custom frontend
// can ignore --network=none altogether. So before ANY build call the runner
// refuses, category C with a named reason, every Dockerfile shape through
// which the build could reach the network or escape the build sandbox, and
// collects every base image for the allowlist check.
//
// The scan is DENY BY DEFAULT and errs toward over-refusal. It first refuses
// every byte on which this scan and the builder could TOKENIZE a line
// differently (checkCharacters: BuildKit splits an instruction keyword on
// [\t\v\f\r ]+ and trims with unicode.IsSpace), so only space and tab
// separate tokens. It then scans the union of three views of the file —
// BuildKit-faithful logical instructions (continuations joined, heredoc
// bodies consumed; a line whose first token is not an instruction is
// REFUSED, as the builder rejects it too), heredoc-UNAWARE logical
// instructions (every line treated as instruction text), and every physical
// line on its own — so a continuation or heredoc the builder groups
// differently from this parser surfaces in at least one view. Flag tokens
// carrying a quote or backslash are refused, because the builder's flag
// lexer rewrites them (and joins a quoted space into one flag), and so is
// any byte >= 0x80 on an instruction line, because that lexer walks the line
// BYTE by byte and ends a word on 0x85 and 0xA0 — UTF-8 continuation bytes
// (`à` is C3 A0) — so it could split a flag this scan reads as one. That is a
// best-effort hedge, NOT a proof of equivalence with every builder (BuildKit,
// buildah, a future frontend): it is designed so a divergence over-refuses,
// and the residuals are documented in the gateiso README.

// maxDockerfileBytes bounds a Dockerfile the scan accepts.
const maxDockerfileBytes = 1 << 20

// DockerfileRefusal is a static refusal: Line is the 1-based physical line
// the finding was made on (0 for a whole-file finding).
type DockerfileRefusal struct {
	Line   int
	Reason string
}

func (r *DockerfileRefusal) Error() string {
	if r.Line > 0 {
		return fmt.Sprintf("Dockerfile line %d: %s", r.Line, r.Reason)
	}
	return "Dockerfile: " + r.Reason
}

func refuseLine(line int, format string, args ...any) *DockerfileRefusal {
	return &DockerfileRefusal{Line: line, Reason: fmt.Sprintf(format, args...)}
}

// Base kinds: where a build base image reference was found.
const (
	BaseFrom      = "FROM"
	BaseCopyFrom  = "COPY --from"
	BaseMountFrom = "RUN --mount from"
)

// BuildBase is one image the build pulls through the daemon.
type BuildBase struct {
	Ref  string
	Line int
	Kind string
}

// Dockerfile is a scanned Dockerfile that passed every profile-independent
// refusal.
type Dockerfile struct {
	// Bases is every image reference the build would pull (FROM, COPY
	// --from=<image>, RUN --mount from=<image>), deduplicated by (Kind, Ref),
	// in first-seen order. Stage names, stage indexes and `scratch` are not
	// bases.
	Bases []BuildBase
	// CacheMountLines are the lines carrying RUN --mount=type=cache (refused
	// under the hosted profile by CheckBuildProfile).
	CacheMountLines []int
}

// dockerfileInstructions is the complete Dockerfile instruction set. The
// keyword is case-insensitive.
var dockerfileInstructions = map[string]bool{
	"ADD": true, "ARG": true, "CMD": true, "COPY": true, "ENTRYPOINT": true,
	"ENV": true, "EXPOSE": true, "FROM": true, "HEALTHCHECK": true,
	"LABEL": true, "MAINTAINER": true, "ONBUILD": true, "RUN": true,
	"SHELL": true, "STOPSIGNAL": true, "USER": true, "VOLUME": true,
	"WORKDIR": true,
}

var (
	// directiveRE matches a parser-directive-shaped line in any comment
	// style BuildKit's syntax detection reads (`#` and `//`).
	directiveRE = regexp.MustCompile(`^(?:#|//)\s*([A-Za-z][A-Za-z0-9]*)\s*=`)
	// syntaxAnywhereRE matches a `syntax` directive anywhere in the file.
	syntaxAnywhereRE = regexp.MustCompile(`(?i)^(?:#|//)\s*syntax\s*=`)
	// continuationRE is BuildKit's line-continuation rule for the default
	// escape character: a backslash followed only by spaces/tabs.
	continuationRE = regexp.MustCompile(`\\[ \t]*$`)
	// heredocTokenRE is a standalone heredoc token: preceded by the start or
	// whitespace (plus an optional fd number), followed by whitespace or the
	// end.
	heredocTokenRE = regexp.MustCompile(`(?:^|[ \t])[0-9]*<<(-?)(["']?)([A-Za-z0-9_]+)(["']?)(?:[ \t]|$)`)
	// cleanSourceRE is the deny-by-default character set of an ADD/COPY
	// build-context source: no quote, backslash, `$`, `:`, `@`, `#`, `{`,
	// whitespace or other shell-significant byte.
	cleanSourceRE = regexp.MustCompile(`^[A-Za-z0-9._/*?\[\]+=,~-]+$`)
	// stageNameRE is BuildKit's stage-name grammar (compared lowercase).
	stageNameRE = regexp.MustCompile(`^[a-z][a-z0-9._-]*$`)
	// heredocSourceRE is an ADD/COPY source that is a heredoc marker.
	heredocSourceRE = regexp.MustCompile(`^<<-?["']?[A-Za-z0-9_]+["']?$`)
)

// allowedDirectives are the parser directives that cannot select a frontend
// or change how this scan tokenizes the file. `escape` is NOT allowed: it
// changes the continuation character the scan relies on.
var allowedDirectives = map[string]bool{"check": true}

// physLine is one physical line with its 1-based number.
type physLine struct {
	n    int
	text string
}

// logicalInstr is one instruction: keyword (upper-cased), the raw text after
// the keyword, and the physical line it started on.
type logicalInstr struct {
	line    int
	keyword string
	rest    string
}

// ParseDockerfile runs every profile-independent static refusal over content
// and collects the build bases. A refusal is returned as *DockerfileRefusal.
//
// Refused: content over 1 MiB or not valid UTF-8; a control character other
// than tab and newline, a CR not followed by LF, or non-ASCII whitespace (see
// checkCharacters); a logical instruction whose first token is not a
// Dockerfile instruction; a byte >= 0x80 on an instruction line (outside
// comment lines and consumed heredoc bodies) or in any view's flag token; a
// flag token carrying a quote or backslash; any leading parser
// directive other than `check` (so `# syntax=`, `# escape=`, an unknown
// directive) and a `syntax` directive anywhere; a first significant line that
// is neither a `#` comment nor a Dockerfile instruction (`// syntax=`, a JSON
// `{"syntax": …}` first line); an ADD source or a plain COPY source that is
// not a clean, relative, in-context path (see checkAddSource /
// checkCopySource); RUN --network other than none; RUN --security=insecure;
// a --from / --mount from= value carrying a URL scheme; an unparsable mount.
func ParseDockerfile(content []byte) (*Dockerfile, error) {
	if len(content) > maxDockerfileBytes {
		return nil, refuseLine(0, "is %d bytes, over the %d-byte limit", len(content), maxDockerfileBytes)
	}
	content = bytes.TrimPrefix(content, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(content) {
		return nil, refuseLine(0, "is not valid UTF-8")
	}
	if err := checkCharacters(string(content)); err != nil {
		return nil, err
	}
	lines := splitPhysical(string(content))
	if err := checkDirectives(lines); err != nil {
		return nil, err
	}
	viewA, err := logicalInstructions(lines, true)
	if err != nil {
		return nil, err
	}
	viewB, _ := logicalInstructions(lines, false)
	views := [][]logicalInstr{viewA, viewB, physicalInstructions(lines)}
	df := &Dockerfile{}
	seenBase := map[string]bool{}
	seenCache := map[int]bool{}
	// Stage names come ONLY from the BuildKit-faithful view, declared
	// progressively there; the two backstop views read its final set and
	// never declare one, so a `FROM … AS name` the builder consumes as a
	// heredoc body can never turn a later image reference into a "stage".
	stages := map[string]bool{}
	for vi, view := range views {
		for _, in := range view {
			bases, cacheLine, err := checkInstruction(in, stages, vi == 0)
			if err != nil {
				return nil, err
			}
			for _, b := range bases {
				k := b.Kind + "\x00" + b.Ref
				if !seenBase[k] {
					seenBase[k] = true
					df.Bases = append(df.Bases, b)
				}
			}
			if cacheLine > 0 && !seenCache[cacheLine] {
				seenCache[cacheLine] = true
				df.CacheMountLines = append(df.CacheMountLines, cacheLine)
			}
		}
	}
	return df, nil
}

// checkCharacters refuses every character on which this scan and the
// builder could split a line differently. BuildKit separates an instruction
// keyword from its arguments on [\t\v\f\r ]+ and trims leading whitespace
// with unicode.IsSpace, while this scan separates on space and tab only: a
// keyword followed by a vertical tab, form feed or bare CR (`ADD<VT>url`,
// `FROM<FF>image`) is an instruction to the builder and an unknown word here.
// So a control character other than tab and newline, a CR not immediately
// followed by LF, and any non-ASCII whitespace are refused outright, leaving
// space and tab as the only separators either side can see.
func checkCharacters(s string) error {
	line := 1
	for i, r := range s {
		switch {
		case r == '\n':
			line++
		case r == '\t', r == '\r' && strings.HasPrefix(s[i+1:], "\n"):
		case unicode.IsControl(r) || (r > unicode.MaxASCII && unicode.IsSpace(r)):
			return refuseLine(line, "character %U is refused: only space and tab may separate tokens (the builder also splits on vertical tab, form feed, a bare CR and Unicode whitespace, which this scan does not)", r)
		}
	}
	return nil
}

// splitPhysical splits on \n, dropping a trailing \r as bufio.ScanLines
// does (checkCharacters has refused every other CR).
func splitPhysical(s string) []physLine {
	var out []physLine
	for i, l := range strings.Split(s, "\n") {
		out = append(out, physLine{n: i + 1, text: strings.TrimSuffix(l, "\r")})
	}
	return out
}

// checkDirectives refuses every directive shape that can select a frontend
// or change tokenization: a `syntax` directive on ANY line, any leading
// directive other than `check`, and a first significant line (after blank
// lines and a #! shebang) that is neither a `#` comment nor an instruction.
func checkDirectives(lines []physLine) error {
	for _, l := range lines {
		if syntaxAnywhereRE.MatchString(strings.TrimSpace(l.text)) {
			return refuseLine(l.n, "a `syntax` parser directive selects a BuildKit frontend image, which can ignore --network=none; remove it")
		}
	}
	firstSignificant := true
	for i, l := range lines {
		t := strings.TrimSpace(l.text)
		if t == "" {
			continue
		}
		if i == 0 && strings.HasPrefix(t, "#!") {
			continue
		}
		if firstSignificant {
			firstSignificant = false
			if !strings.HasPrefix(t, "#") && !dockerfileInstructions[strings.ToUpper(firstWord(t))] {
				return refuseLine(l.n, "the first line is neither a `#` comment nor a Dockerfile instruction (a `//` or JSON parser directive can select a frontend)")
			}
		}
		m := directiveRE.FindStringSubmatch(t)
		if m == nil {
			if strings.HasPrefix(t, "#") {
				continue // an ordinary comment; directives may still follow only per this scan's over-refusal
			}
			return nil // the first instruction ends the directive block
		}
		if !allowedDirectives[strings.ToLower(m[1])] {
			return refuseLine(l.n, "parser directive %q is refused (only `check` is permitted: a directive can select a frontend or change how the file is tokenized)", strings.ToLower(m[1]))
		}
	}
	return nil
}

func firstWord(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}

// firstNonASCII returns the first character of s whose UTF-8 encoding holds
// a byte >= 0x80.
func firstNonASCII(s string) (rune, bool) {
	for _, r := range s {
		if r > unicode.MaxASCII {
			return r, true
		}
	}
	return 0, false
}

// nonASCIIRefusal refuses a byte >= 0x80 on an instruction line. BuildKit's
// flag lexer walks the line byte by byte and ends a word on
// unicode.IsSpace(rune(byte)), which is true for 0x85 and 0xA0 — the
// continuation bytes of `ą` (C4 85) and `à` (C3 A0) — so
// `--mount=…,target=/tà--mount=from=<image>,…` is TWO mounts to the builder
// and one flag token here. Refusing every non-ASCII byte on an instruction
// line (comment lines and consumed heredoc bodies keep them) leaves no byte
// the two lexers split differently.
func nonASCIIRefusal(line int, s string) *DockerfileRefusal {
	r, ok := firstNonASCII(s)
	if !ok {
		return nil
	}
	return refuseLine(line, "character %U on an instruction line is refused: the builder's flag lexer reads the line byte by byte and ends a word on the UTF-8 bytes 0x85 and 0xA0, so a non-ASCII character can split a flag this scan reads as one (keep non-ASCII text in comments or a heredoc body)", r)
}

// logicalInstructions groups physical lines into instructions the way
// BuildKit's parser does for the default escape character: comment lines
// are skipped (also inside a continuation), a line ending in `\` plus
// optional spaces/tabs continues, empty continuation lines are skipped. With
// heredocAware, the body lines of each heredoc an ADD/COPY/RUN opens are
// consumed up to their terminator, as BuildKit does, and any `<<` in such an
// instruction that is not a plain standalone heredoc token is REFUSED (a
// heredoc this parser and the builder delimit differently is the one way the
// faithful view could see an instruction the builder does not), and a logical
// line whose first token is not an instruction is REFUSED rather than skipped
// (the builder rejects an unknown instruction, so this refuses nothing the
// builder would build, and a keyword this scan failed to recognise can never
// pass silently), as is a byte >= 0x80 on any physical line the instruction
// spans (see nonASCIIRefusal); without heredocAware every line is
// instruction text and an unknown first token is skipped — the backstop view
// against a heredoc this parser detects but the builder does not.
func logicalInstructions(lines []physLine, heredocAware bool) ([]logicalInstr, error) {
	var out []logicalInstr
	for i := 0; i < len(lines); i++ {
		t := strings.TrimLeft(lines[i].text, " \t")
		if strings.HasPrefix(t, "#") || strings.TrimSpace(t) == "" {
			continue
		}
		start := lines[i].n
		var wide *DockerfileRefusal
		if heredocAware {
			wide = nonASCIIRefusal(start, t)
		}
		text, more := trimContinuation(t)
		for more && i+1 < len(lines) {
			i++
			next := lines[i].text
			nt := strings.TrimLeft(next, " \t")
			if strings.HasPrefix(nt, "#") || strings.TrimSpace(nt) == "" {
				continue
			}
			if heredocAware && wide == nil {
				wide = nonASCIIRefusal(lines[i].n, next)
			}
			var part string
			part, more = trimContinuation(next)
			text += part
		}
		kw, rest := splitKeyword(text)
		if kw == "" {
			if heredocAware {
				return nil, refuseLine(start, "%q is not a Dockerfile instruction (an unrecognised first token is refused, never skipped)", firstWord(text))
			}
			continue
		}
		if wide != nil {
			return nil, wide
		}
		out = append(out, logicalInstr{line: start, keyword: kw, rest: rest})
		// ONBUILD ADD/COPY/RUN opens a heredoc exactly as the bare form does.
		hkw, hrest := kw, rest
		if kw == "ONBUILD" {
			hkw, hrest = splitKeyword(rest)
		}
		if !heredocAware || (hkw != "ADD" && hkw != "COPY" && hkw != "RUN") || !strings.Contains(hrest, "<<") {
			continue
		}
		docs, err := heredocTokens(start, hrest)
		if err != nil {
			return nil, err
		}
		for _, d := range docs {
			for i+1 < len(lines) {
				i++
				body := lines[i].text
				if d.chomp {
					body = strings.TrimLeft(body, "\t")
				}
				if body == d.word {
					break
				}
			}
		}
	}
	return out, nil
}

type heredoc struct {
	chomp bool
	word  string
}

// heredocTokens returns the heredocs an instruction opens. Every `<<` must
// belong to a standalone token `[fd]<<[-]WORD`, `"WORD"` or `'WORD'` with
// WORD in [A-Za-z0-9_]+; anything else (`<<<`, `"<<EOF"`, `<<EOF.x`,
// `<<$VAR`, `cat<<EOF`) is refused.
func heredocTokens(line int, rest string) ([]heredoc, error) {
	var docs []heredoc
	for _, m := range heredocTokenRE.FindAllStringSubmatch(rest, -1) {
		if m[2] != m[4] {
			return nil, refuseLine(line, "heredoc token %q has mismatched quotes", strings.TrimSpace(m[0]))
		}
		docs = append(docs, heredoc{chomp: m[1] == "-", word: m[3]})
	}
	if strings.Count(rest, "<<") != len(docs) {
		return nil, refuseLine(line, "a `<<` that is not a plain standalone heredoc token (<<WORD, <<-WORD, <<\"WORD\", WORD in [A-Za-z0-9_]) is refused: the builder could delimit it differently from this scan")
	}
	return docs, nil
}

// physicalInstructions treats every physical line on its own as an
// instruction when its first word is an instruction keyword, with no
// continuation joining and no heredoc skipping.
func physicalInstructions(lines []physLine) []logicalInstr {
	var out []logicalInstr
	for _, l := range lines {
		t := strings.TrimSpace(l.text)
		if strings.HasPrefix(t, "#") {
			continue
		}
		t, _ = trimContinuation(t)
		if kw, rest := splitKeyword(t); kw != "" {
			out = append(out, logicalInstr{line: l.n, keyword: kw, rest: rest})
		}
	}
	return out
}

func trimContinuation(s string) (string, bool) {
	if loc := continuationRE.FindStringIndex(s); loc != nil {
		return s[:loc[0]], true
	}
	return s, false
}

// splitKeyword returns the upper-cased instruction keyword and the rest of
// the instruction; "" when the first word is not an instruction.
func splitKeyword(s string) (string, string) {
	s = strings.TrimLeft(s, " \t")
	i := strings.IndexAny(s, " \t")
	word, rest := s, ""
	if i >= 0 {
		word, rest = s[:i], strings.TrimLeft(s[i:], " \t")
	}
	kw := strings.ToUpper(word)
	if !dockerfileInstructions[kw] {
		return "", ""
	}
	return kw, rest
}

// splitFlags splits leading `--flag[=value]` tokens from the arguments. A
// flag token carrying a quote or backslash is refused: the builder's flag
// lexer strips quotes and escapes and keeps a quoted space inside the flag
// (`--mount="type=bind, from=evil/x"` is ONE flag to it, two words here), so
// this scan could not classify the value the builder uses. A flag token
// carrying a byte >= 0x80 is refused in EVERY view (the faithful view has
// already refused it line-wide, see nonASCIIRefusal): it covers the backstop
// views, where a line the faithful view consumes as a heredoc body is an
// instruction to a builder that delimits the heredoc differently.
func splitFlags(line int, rest string) ([]string, string, error) {
	var flags []string
	s := strings.TrimLeft(rest, " \t")
	for strings.HasPrefix(s, "--") {
		i := strings.IndexAny(s, " \t")
		tok := s
		if i < 0 {
			s = ""
		} else {
			tok, s = s[:i], strings.TrimLeft(s[i:], " \t")
		}
		if tok == "--" {
			break
		}
		if strings.ContainsAny(tok, `"'\`) {
			return nil, "", refuseLine(line, "flag %q refused: a quote or backslash in a flag is rewritten by the builder's flag lexer, so the flag cannot be classified statically", tok)
		}
		if r, ok := firstNonASCII(tok); ok {
			return nil, "", refuseLine(line, "flag %q refused: character %U carries a byte >= 0x80, and the builder's flag lexer ends a word on 0x85 and 0xA0, so the flag cannot be classified statically", tok, r)
		}
		flags = append(flags, tok)
	}
	return flags, s, nil
}

func flagValue(flag, name string) (string, bool) {
	k, v, ok := strings.Cut(flag, "=")
	if strings.ToLower(k) != "--"+name {
		return "", false
	}
	if !ok {
		return "", true
	}
	return v, true
}

// argList parses an ADD/COPY argument string the way BuildKit does: a JSON
// array when the text starts with `[` and decodes as one (trailing text after
// the array is refused), else whitespace-split tokens.
func argList(line int, s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") {
		dec := json.NewDecoder(strings.NewReader(s))
		var arr []any
		if err := dec.Decode(&arr); err == nil {
			if strings.TrimSpace(s[dec.InputOffset():]) != "" {
				return nil, refuseLine(line, "text after a JSON-form argument list is refused")
			}
			out := make([]string, 0, len(arr))
			for _, a := range arr {
				str, ok := a.(string)
				if !ok {
					return nil, refuseLine(line, "a JSON-form argument list must hold only strings")
				}
				out = append(out, str)
			}
			return out, nil
		}
	}
	return strings.Fields(s), nil
}

// checkInstruction applies the per-instruction refusals and returns the bases
// and the cache-mount line it contributes. stages is the stage-name set; a
// FROM … AS name adds to it only when declare is set.
func checkInstruction(in logicalInstr, stages map[string]bool, declare bool) ([]BuildBase, int, error) {
	kw, rest := in.keyword, in.rest
	if kw == "ONBUILD" {
		inner, innerRest := splitKeyword(rest)
		if inner == "" {
			return nil, 0, nil
		}
		kw, rest = inner, innerRest
	}
	switch kw {
	case "FROM":
		return checkFrom(in.line, rest, stages, declare)
	case "ADD", "COPY":
		return checkAddCopy(in.line, kw, rest, stages)
	case "RUN":
		return checkRun(in.line, rest, stages)
	}
	return nil, 0, nil
}

func checkFrom(line int, rest string, stages map[string]bool, declare bool) ([]BuildBase, int, error) {
	_, args, err := splitFlags(line, rest)
	if err != nil {
		return nil, 0, err
	}
	f := strings.Fields(args)
	if len(f) == 0 {
		return nil, 0, nil
	}
	img := f[0]
	var bases []BuildBase
	lower := strings.ToLower(img)
	if strings.Contains(img, "://") {
		return nil, 0, refuseLine(line, "FROM %q carries a URL scheme", img)
	}
	if lower != "scratch" && !stages[lower] {
		bases = append(bases, BuildBase{Ref: img, Line: line, Kind: BaseFrom})
	}
	if declare && len(f) >= 3 && strings.EqualFold(f[1], "AS") {
		if name := strings.ToLower(f[2]); stageNameRE.MatchString(name) {
			stages[name] = true
		}
	}
	return bases, 0, nil
}

// stageRef reports whether a --from / from= value names a build stage (a
// stage declared earlier in this view or, with numericIndex, a numeric stage
// index) rather than an image. Only COPY --from resolves a numeric index:
// BuildKit resolves RUN --mount from= by stage NAME alone, so a digits-only
// mount source is an IMAGE (`from=0` pulls docker.io/library/0) and must
// reach CheckBuildBases.
func stageRef(v string, stages map[string]bool, numericIndex bool) bool {
	if v == "" {
		return true
	}
	allDigits := true
	for _, r := range v {
		if r < '0' || r > '9' {
			allDigits = false
			break
		}
	}
	return (numericIndex && allDigits) || stages[strings.ToLower(v)]
}

func checkAddCopy(line int, kw, rest string, stages map[string]bool) ([]BuildBase, int, error) {
	flags, args, err := splitFlags(line, rest)
	if err != nil {
		return nil, 0, err
	}
	var bases []BuildBase
	from := ""
	hasFrom := false
	for _, fl := range flags {
		if v, ok := flagValue(fl, "from"); ok {
			hasFrom, from = true, v
		}
	}
	if hasFrom {
		if strings.Contains(from, "://") {
			return nil, 0, refuseLine(line, "%s --from=%q carries a URL scheme", kw, from)
		}
		if !stageRef(from, stages, true) {
			bases = append(bases, BuildBase{Ref: from, Line: line, Kind: BaseCopyFrom})
		}
	}
	list, err := argList(line, args)
	if err != nil {
		return nil, 0, err
	}
	if len(list) < 2 {
		return bases, 0, nil
	}
	for _, src := range list[:len(list)-1] {
		switch {
		case heredocSourceRE.MatchString(src):
			continue
		case kw == "ADD":
			if reason := checkAddSource(src); reason != "" {
				return nil, 0, refuseLine(line, "ADD source %q refused: %s; ADD may name only a plain relative path inside the build context (use COPY for local files)", src, reason)
			}
		case !hasFrom:
			if reason := checkCopySource(src); reason != "" {
				return nil, 0, refuseLine(line, "COPY source %q refused: %s; COPY may name only a plain relative path inside the build context", src, reason)
			}
		}
	}
	return bases, 0, nil
}

// checkCopySource is the deny-by-default rule for a build-context COPY
// source: not empty, no variable expansion, no URL, not absolute, no
// backslash, no `..` segment, and nothing outside cleanSourceRE's character
// set (a shell-form quote such as `repo.g"i"t` is removed by the builder
// before it classifies the source). It returns "" when accepted.
func checkCopySource(src string) string {
	switch {
	case src == "":
		return "empty source"
	case strings.Contains(src, "$"):
		return "variable expansion ($) is resolved before the builder classifies the source"
	case strings.Contains(src, "://"):
		return "a URL is not a build-context path"
	case strings.HasPrefix(src, "/"):
		return "an absolute path is not a clean relative path"
	case strings.Contains(src, `\`):
		return "a backslash is not a clean relative path"
	}
	for _, seg := range strings.Split(src, "/") {
		if seg == ".." {
			return "a '..' segment leaves the build context"
		}
	}
	if !cleanSourceRE.MatchString(src) {
		return "only letters, digits and . _ - / * ? [ ] + = , ~ are permitted (quotes and other shell-significant characters are rewritten by the builder before it classifies the source)"
	}
	return ""
}

// checkAddSource is the deny-by-default rule for an ADD source. BuildKit
// classifies an ADD source as REMOTE (fetched by the builder, outside
// --network=none) when it is an http(s) URL or parses as a git remote — and
// its git-remote parser accepts scheme-less `host/path.git`, `github.com/…`,
// and scp-like `user@host:path`. So beyond the COPY rule an ADD source is
// refused when it contains `://`, `@`, `.git` or `:`, or its first path
// component contains a dot (host-shaped), with `.` itself the only exception.
// When in doubt this refuses.
func checkAddSource(src string) string {
	lower := strings.ToLower(src)
	switch {
	case strings.Contains(lower, "://"):
		return "a URL is fetched by the builder outside --network=none"
	case strings.Contains(lower, "@"):
		return "'@' can name a git remote (user@host:path) or a ref"
	case strings.Contains(lower, ".git"):
		return "'.git' can name a git remote the builder fetches"
	case strings.Contains(lower, ":"):
		return "':' can name an scp-like remote (host:path) or a port"
	}
	if reason := checkCopySource(src); reason != "" {
		return reason
	}
	rest := lower
	for strings.HasPrefix(rest, "./") {
		rest = strings.TrimLeft(strings.TrimPrefix(rest, "./"), "/")
	}
	first, _, _ := strings.Cut(rest, "/")
	if first != "." && strings.Contains(first, ".") {
		return "a first path component containing a dot is host-shaped and can name a remote"
	}
	return ""
}

func checkRun(line int, rest string, stages map[string]bool) ([]BuildBase, int, error) {
	flags, _, err := splitFlags(line, rest)
	if err != nil {
		return nil, 0, err
	}
	var bases []BuildBase
	cacheLine := 0
	for _, fl := range flags {
		if v, ok := flagValue(fl, "network"); ok {
			if strings.ToLower(strings.TrimSpace(v)) != "none" {
				return nil, 0, refuseLine(line, "RUN --network=%s refused: only --network=none is permitted in a gate image build", v)
			}
		}
		if v, ok := flagValue(fl, "security"); ok && strings.ToLower(strings.TrimSpace(v)) != "sandbox" {
			return nil, 0, refuseLine(line, "RUN --security=%s refused: a gate image build runs sandboxed", v)
		}
		v, ok := flagValue(fl, "mount")
		if !ok {
			continue
		}
		m, err := parseMount(v)
		if err != nil {
			return nil, 0, refuseLine(line, "RUN --mount=%q refused: %v", v, err)
		}
		for _, f := range m["from"] {
			if strings.Contains(f, "://") {
				return nil, 0, refuseLine(line, "RUN --mount from=%q carries a URL scheme", f)
			}
			if !stageRef(f, stages, false) {
				bases = append(bases, BuildBase{Ref: f, Line: line, Kind: BaseMountFrom})
			}
		}
		for _, t := range m["type"] {
			if strings.EqualFold(t, "cache") {
				cacheLine = line
			}
		}
	}
	return bases, cacheLine, nil
}

// parseMount parses a --mount value as BuildKit does (CSV of key=value) and,
// as a union backstop, also by a naive comma split; it returns every value
// seen per lower-cased key.
func parseMount(v string) (map[string][]string, error) {
	r := csv.NewReader(strings.NewReader(v))
	fields, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("unparsable mount: %w", err)
	}
	out := map[string][]string{}
	add := func(kv string) {
		k, val, _ := strings.Cut(kv, "=")
		k = strings.ToLower(strings.TrimSpace(k))
		out[k] = append(out[k], strings.Trim(strings.TrimSpace(val), `"'`))
	}
	for _, f := range fields {
		add(f)
	}
	for _, f := range strings.Split(v, ",") {
		add(f)
	}
	return out, nil
}

// CheckBuildProfile applies the profile-dependent refusals: under hosted (and
// any strict profile) a RUN --mount=type=cache is refused, because a cache
// mount persists on the shared daemon across builds and projects (cross-
// project cache poisoning). Elsewhere it is an accepted, documented limit.
func CheckBuildProfile(df *Dockerfile, profile Profile) error {
	if strictProfile(profile) && len(df.CacheMountLines) > 0 {
		return refuseLine(df.CacheMountLines[0], "RUN --mount=type=cache is refused under profile %s: a cache mount persists on the shared daemon across builds and projects", profile)
	}
	return nil
}

// CheckBuildBases refuses, when an allowlist is configured, any base that is
// variable-bearing (`$`: not statically verifiable), unparsable, or not
// permitted; and, when requirePinned, any base that is not digest-pinned.
// With an empty allowlist and requirePinned false every base passes.
func CheckBuildBases(df *Dockerfile, allow Allowlist, requirePinned bool) error {
	if allow.Empty() && !requirePinned {
		return nil
	}
	for _, b := range df.Bases {
		if strings.Contains(b.Ref, "$") {
			return refuseLine(b.Line, "%s base %q uses variable expansion and cannot be checked against the operator image allowlist", b.Kind, b.Ref)
		}
		ref, err := ParseImageRef(b.Ref)
		if err != nil {
			return refuseLine(b.Line, "%s base %q is not a valid image reference: %v", b.Kind, b.Ref, err)
		}
		if !allow.Empty() && !allow.Permits(ref) {
			return refuseLine(b.Line, "%s base %s is not permitted by the operator image allowlist (FISHHAWK_GATE_IMAGE_ALLOWLIST)", b.Kind, ref)
		}
		if requirePinned && !ref.Pinned() {
			return refuseLine(b.Line, "%s base %s is not digest-pinned: write %s@sha256:<digest>", b.Kind, ref, ref.Name())
		}
	}
	return nil
}

// ScreenDockerfile is the one call the runner makes on the committed
// Dockerfile bytes BEFORE any build: ParseDockerfile, CheckBuildProfile, then
// CheckBuildBases under the policy decision's BasesMustPass /
// RequirePinnedBases. Any error is a *DockerfileRefusal (gateRefused).
func ScreenDockerfile(content []byte, profile Profile, allow Allowlist, d ImageDecision) error {
	df, err := ParseDockerfile(content)
	if err != nil {
		return err
	}
	if err := CheckBuildProfile(df, profile); err != nil {
		return err
	}
	if d.BasesMustPass || d.RequirePinnedBases {
		return CheckBuildBases(df, allow, d.RequirePinnedBases)
	}
	return nil
}
