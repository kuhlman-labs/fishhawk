package gateiso

import (
	"errors"
	"strings"
	"testing"
)

func refusalOf(t *testing.T, content string) *DockerfileRefusal {
	t.Helper()
	_, err := ParseDockerfile([]byte(content))
	if err == nil {
		return nil
	}
	var r *DockerfileRefusal
	if !errors.As(err, &r) {
		t.Fatalf("ParseDockerfile error %v is not a *DockerfileRefusal", err)
	}
	return r
}

func TestParseDockerfile_Refusals(t *testing.T) {
	rows := []struct {
		name, content, want string
	}{
		// Parser directives (condition 1).
		{"syntax directive at top", "# syntax=docker/dockerfile:1\nFROM alpine\n", "syntax"},
		{"syntax spaced uppercase", "#  SYNTAX = x\nFROM alpine\n", "syntax"},
		{"syntax after a comment block", "# hello\n# world\n# syntax=x\nFROM alpine\n", "syntax"},
		{"syntax mid-file", "FROM alpine\n# syntax=x\nRUN true\n", "syntax"},
		{"c-style syntax directive", "// syntax=evil/frontend\nFROM alpine\n", "syntax"},
		{"c-style other directive", "// foo=bar\nFROM alpine\n", "first line is neither"},
		{"json first line", "{\"syntax\": \"evil/frontend\"}\nFROM alpine\n", "first line is neither"},
		{"json after shebang", "#!/usr/bin/env x\n{\"syntax\": \"evil\"}\nFROM alpine\n", "first line is neither"},
		{"escape directive", "# escape=`\nFROM alpine\n", `parser directive "escape"`},
		{"unknown directive", "# frontend=x\nFROM alpine\n", `parser directive "frontend"`},
		{"uppercase unknown directive", "# FOO=x\nFROM alpine\n", `parser directive "foo"`},
		// ADD remote sources, every case (conditions 1 and 2).
		{"ADD https", "FROM alpine\nADD https://x/y /z\n", "ADD source"},
		{"add http lowercase", "FROM alpine\nadd http://x/y /z\n", "ADD source"},
		{"Add git scheme mixed case", "FROM alpine\nAdd git://h/r /s\n", "ADD source"},
		{"ADD scp-like", "FROM alpine\nADD git@github.com:o/r.git /src\n", "ADD source"},
		{"ADD git url with ref", "FROM alpine\nADD https://github.com/o/r.git#main /src\n", "ADD source"},
		{"ADD scheme-less github", "FROM alpine\nADD github.com/o/r /src\n", "host-shaped"},
		{"ADD scheme-less .git", "FROM alpine\nADD example/r.git /src\n", ".git"},
		{"ADD at sign", "FROM alpine\nADD a@b /src\n", "'@'"},
		{"ADD colon", "FROM alpine\nADD host:repo /x\n", "':'"},
		{"ADD dollar", "FROM alpine\nARG URL=https://x\nADD $URL /x\n", "variable expansion"},
		{"ADD braced dollar", "FROM alpine\nADD ${URL} /x\n", "variable expansion"},
		{"ADD shell-quoted .git", "FROM alpine\nADD myhost/repo.g''it /x\n", "only letters"},
		{"ADD dotted first component", "FROM alpine\nADD foo.tar /x\n", "host-shaped"},
		{"ADD dot-slash dotted component", "FROM alpine\nADD ././foo.tar /x\n", "host-shaped"},
		{"ADD absolute", "FROM alpine\nADD /etc/passwd /x\n", "absolute"},
		{"ADD dotdot", "FROM alpine\nADD ../x /y\n", "'..'"},
		{"ADD backslash", "FROM alpine\nADD a\\b /y\n", "backslash"},
		{"ADD json", "FROM alpine\nADD [\"https://x\", \"/y\"]\n", "ADD source"},
		{"ADD json escaped scheme", "FROM alpine\nADD [\"\\u0068ttps://x\", \"/y\"]\n", "ADD source"},
		{"ADD json trailing text", "FROM alpine\nADD [\"a\", \"/b\"] https://x /c\n", "text after a JSON-form"},
		{"ADD json non-string", "FROM alpine\nADD [1, \"/y\"]\n", "only strings"},
		{"ONBUILD ADD", "FROM alpine\nONBUILD ADD https://x /y\n", "ADD source"},
		{"onbuild add lowercase", "FROM alpine\nonbuild add https://x /y\n", "ADD source"},
		{"ADD with checksum flag", "FROM alpine\nADD --checksum=sha256:abc https://x /y\n", "ADD source"},
		{"ADD after flag terminator", "FROM alpine\nADD -- https://x /y\n", "ADD source"},
		{"ADD continuation", "FROM alpine\nADD \\\n  https://x /y\n", "ADD source"},
		// COPY build-context sources.
		{"COPY dollar", "FROM alpine\nCOPY $SRC /x\n", "COPY source"},
		{"copy absolute lowercase", "FROM alpine\ncopy /abs /x\n", "COPY source"},
		{"COPY dotdot", "FROM alpine\nCOPY ../x /y\n", "COPY source"},
		{"COPY url", "FROM alpine\nCOPY https://x /y\n", "COPY source"},
		{"COPY quoted", "FROM alpine\nCOPY \"a b\" /x\n", "COPY source"},
		{"COPY --from url", "FROM alpine\nCOPY --from=https://x/y /a /b\n", "URL scheme"},
		{"FROM url", "FROM docker-image://alpine\n", "URL scheme"},
		// RUN network/security/mount.
		{"RUN network host", "FROM alpine\nRUN --network=host true\n", "--network=host"},
		{"Run network host mixed case", "FROM alpine\nRun --network=host true\n", "--network=host"},
		{"run network default lowercase", "FROM alpine\nrun --network=default true\n", "--network=default"},
		{"RUN network empty", "FROM alpine\nRUN --network true\n", "--network="},
		{"RUN security insecure", "FROM alpine\nRUN --security=insecure true\n", "--security=insecure"},
		{"rUn security insecure", "FROM alpine\nrUn --security=insecure true\n", "--security=insecure"},
		{"RUN network on continuation", "FROM alpine\nRUN \\\n--network=host true\n", "--network=host"},
		{"RUN comment inside continuation", "FROM alpine\nRUN \\\n# note\n--network=host true\n", "--network=host"},
		{"RUN mount from url", "FROM alpine\nRUN --mount=type=bind,from=https://x,target=/x true\n", "URL scheme"},
		{"RUN mount unparsable", "FROM alpine\nRUN --mount=type=bind,\"from=x true\n", "unparsable mount"},
		// Heredoc tokens the builder could delimit differently.
		{"here-string", "FROM alpine\nRUN cat <<<x\n", "standalone heredoc"},
		{"quoted heredoc marker", "FROM alpine\nRUN echo \"<<EOF\"\n", "standalone heredoc"},
		{"heredoc word with dot", "FROM alpine\nRUN cat <<EOF.x\nEOF.x\n", "standalone heredoc"},
		{"heredoc mismatched quotes", "FROM alpine\nRUN cat <<\"EOF'\nEOF\n", "mismatched quotes"},
		// The backstop views.
		{"physical line after backslash-space", "FROM alpine\nRUN echo hi \\ \nADD https://x /y\n", "ADD source"},
		{"bare CR line split", "FROM alpine\nRUN echo hi\rADD https://x /y\n", "ADD source"},
		{"heredoc-unaware continuation", "FROM alpine\nRUN cat <<EOF\nRUN \\\n  --network=host echo\nEOF\n", "--network=host"},
		// Whole-file.
		{"invalid utf8", "FROM alpine\nRUN \xff\n", "UTF-8"},
		{"oversized", "FROM alpine\n" + strings.Repeat("#", maxDockerfileBytes), "byte limit"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			got := refusalOf(t, r.content)
			if got == nil {
				t.Fatalf("ParseDockerfile accepted:\n%s", r.content)
			}
			if !strings.Contains(got.Error(), r.want) {
				t.Fatalf("refusal = %q, want it to contain %q", got.Error(), r.want)
			}
		})
	}
}

func TestParseDockerfile_Accepts(t *testing.T) {
	rows := map[string]string{
		"typical multi-stage": `#check=skip=all
# A gate image.
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY . .
copy ./internal/ /src/internal/
ADD ./vendor /v
ADD . /all
ADD <<EOF /etc/x
hello https://not-an-instruction
EOF
RUN --network=none go build ./...
RUN --security=sandbox true
RUN --mount=type=bind,target=/x true
COPY --from=build /out/bin /usr/bin/x
COPY --chown=1:1 --from=build /out/a /a
FROM scratch
COPY --from=0 /a /b
COPY <<-EOT /b
	x
	EOT
COPY ["go.mod", "./"]
`,
		"shebang first":                 "#!/usr/bin/env dockerfile\nFROM alpine\n",
		"blank lines first":             "\n\n\nFROM alpine\n",
		"bom":                           "\xef\xbb\xbfFROM alpine\n",
		"crlf":                          "FROM alpine\r\nRUN true\r\n",
		"heredoc with tab chomp and fd": "FROM alpine\nRUN 3<<-EOF cat\n\tset -e\n\tEOF\n",
	}
	for name, c := range rows {
		t.Run(name, func(t *testing.T) {
			if r := refusalOf(t, c); r != nil {
				t.Fatalf("refused: %v", r)
			}
		})
	}
}

func baseRefs(df *Dockerfile) []string {
	var out []string
	for _, b := range df.Bases {
		out = append(out, b.Kind+" "+b.Ref)
	}
	return out
}

func TestParseDockerfile_Bases(t *testing.T) {
	pinned := "ghcr.io/x/y@" + digA
	df, err := ParseDockerfile([]byte(`FROM --platform=$BUILDPLATFORM golang:1.25 AS build
from evil/base AS Second
FROM build
FROM SECOND
COPY --from=build /a /b
copy --from=` + pinned + ` /a /b
COPY --from=0 /a /b
COPY --from=second /a /b
RUN --mount=type=bind,from=alpine,target=/x true
Run --mount=type=cache,target=/c true
run --mount=type=bind,from=build,target=/y true
FROM scratch
FROM Scratch
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"FROM golang:1.25", "FROM evil/base", "COPY --from " + pinned, "RUN --mount from alpine"}
	if got := baseRefs(df); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("bases = %q, want %q", got, want)
	}
	if len(df.CacheMountLines) != 1 || df.CacheMountLines[0] != 10 {
		t.Fatalf("cache mount lines = %v, want [10]", df.CacheMountLines)
	}
	if df.Bases[0].Line != 1 || df.Bases[0].Kind != BaseFrom {
		t.Fatalf("first base = %+v", df.Bases[0])
	}
}

// TestParseDockerfile_StageNameOnlyFromFaithfulView pins that a `FROM … AS
// name` the builder consumes as a heredoc BODY never makes a later
// --from=name a stage reference: the backstop views read the faithful view's
// stage set and never declare one. The fixture isolates the backstop views:
// the quoted `"x <<ZZZ y"` is not a heredoc to BuildKit (its word keeps the
// quotes) but the faithful view treats it as one and swallows the rest of the
// file, so only the heredoc-unaware and physical views see the COPY — and
// both have seen the heredoc-body `FROM scratch AS evil` line first.
func TestParseDockerfile_StageNameOnlyFromFaithfulView(t *testing.T) {
	df, err := ParseDockerfile([]byte("FROM alpine\nRUN cat <<EOF\nFROM scratch AS evil\nEOF\nRUN echo \"x <<ZZZ y\"\nCOPY --from=evil /a /b\n"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range df.Bases {
		if b.Kind == BaseCopyFrom && b.Ref == "evil" {
			found = true
		}
	}
	if !found {
		t.Fatalf("COPY --from=evil not collected as a base: %q", baseRefs(df))
	}
}

func TestCheckBuildBases(t *testing.T) {
	allow, err := ParseAllowlist("ghcr.io/org/")
	if err != nil {
		t.Fatal(err)
	}
	ok := "ghcr.io/org/base@" + digA
	rows := []struct {
		name, content string
		allow         Allowlist
		pinned        bool
		want          string // "" = accepted
	}{
		{"on-list pinned", "FROM " + ok + "\n", allow, true, ""},
		{"off-list FROM", "FROM alpine\n", allow, false, "FROM base docker.io/library/alpine is not permitted"},
		{"off-list lowercase from", "from evil/base\n", allow, false, "FROM base docker.io/evil/base is not permitted"},
		{"off-list COPY --from", "FROM " + ok + "\nCOPY --from=evil/x /a /b\n", allow, false, "COPY --from base docker.io/evil/x is not permitted"},
		{"off-list lowercase copy --from", "FROM " + ok + "\ncopy --from=evil/x /a /b\n", allow, false, "COPY --from base docker.io/evil/x"},
		{"off-list mount from", "FROM " + ok + "\nRUN --mount=type=bind,from=evil/m,target=/x true\n", allow, false, "RUN --mount from base docker.io/evil/m"},
		{"stage names never checked", "FROM " + ok + " AS build\nFROM build\nCOPY --from=build /a /b\nRUN --mount=from=build,target=/x true\n", allow, true, ""},
		{"variable base with allowlist", "ARG B\nFROM $B\n", allow, false, "variable expansion"},
		{"variable base without allowlist", "ARG B\nFROM $B\n", nil, false, ""},
		{"unpinned permitted base, pinning required", "FROM ghcr.io/org/base:1\n", allow, true, "not digest-pinned"},
		{"unpinned permitted base, pinning not required", "FROM ghcr.io/org/base:1\n", allow, false, ""},
		{"unparsable base", "FROM Bad/Ref\n", allow, false, "not a valid image reference"},
		{"no allowlist, no pinning", "FROM anything/at:all\nCOPY --from=x/y /a /b\n", nil, false, ""},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			df, err := ParseDockerfile([]byte(r.content))
			if err != nil {
				t.Fatal(err)
			}
			err = CheckBuildBases(df, r.allow, r.pinned)
			if r.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			var ref *DockerfileRefusal
			if !errors.As(err, &ref) || !strings.Contains(err.Error(), r.want) {
				t.Fatalf("err = %v, want a *DockerfileRefusal containing %q", err, r.want)
			}
		})
	}
}

func TestCheckBuildProfile_CacheMounts(t *testing.T) {
	df, err := ParseDockerfile([]byte("FROM alpine\nrun --mount=type=cache,target=/root/.cache true\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []Profile{ProfileLocal, ProfileSelfHosted} {
		if err := CheckBuildProfile(df, p); err != nil {
			t.Errorf("profile %s: %v", p, err)
		}
	}
	err = CheckBuildProfile(df, ProfileHosted)
	if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "type=cache is refused under profile hosted") {
		t.Fatalf("hosted: err = %v", err)
	}
	clean, _ := ParseDockerfile([]byte("FROM alpine\nRUN --mount=type=bind,target=/x true\n"))
	if err := CheckBuildProfile(clean, ProfileHosted); err != nil {
		t.Fatalf("hosted bind mount: %v", err)
	}
}

func TestScreenDockerfile(t *testing.T) {
	allow, _ := ParseAllowlist("ghcr.io/org/")
	pinnedOK := "FROM ghcr.io/org/b@" + digA + "\n"
	rows := []struct {
		name, content string
		profile       Profile
		d             ImageDecision
		want          string
	}{
		{"local unconstrained", "FROM anything\n", ProfileLocal, ImageDecision{Allowed: true, Build: true}, ""},
		{"parse refusal first", "# syntax=x\nFROM anything\n", ProfileLocal, ImageDecision{Allowed: true, Build: true}, "syntax"},
		{"hosted cache mount", pinnedOK + "RUN --mount=type=cache,target=/c true\n", ProfileHosted, ImageDecision{Allowed: true, Build: true, BasesMustPass: true, RequirePinnedBases: true}, "type=cache"},
		{"hosted unpinned base", "FROM ghcr.io/org/b:1\n", ProfileHosted, ImageDecision{Allowed: true, Build: true, BasesMustPass: true, RequirePinnedBases: true}, "not digest-pinned"},
		{"self-hosted off-list base", "FROM alpine\n", ProfileSelfHosted, ImageDecision{Allowed: true, Build: true, BasesMustPass: true}, "not permitted"},
		{"hosted clean", pinnedOK, ProfileHosted, ImageDecision{Allowed: true, Build: true, BasesMustPass: true, RequirePinnedBases: true}, ""},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			err := ScreenDockerfile([]byte(r.content), r.profile, allow, r.d)
			if r.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			var ref *DockerfileRefusal
			if !errors.As(err, &ref) || !strings.Contains(err.Error(), r.want) {
				t.Fatalf("err = %v, want refusal containing %q", err, r.want)
			}
		})
	}
}

func TestDockerfileRefusal_Error(t *testing.T) {
	if got := (&DockerfileRefusal{Line: 3, Reason: "x"}).Error(); got != "Dockerfile line 3: x" {
		t.Errorf("got %q", got)
	}
	if got := (&DockerfileRefusal{Reason: "y"}).Error(); got != "Dockerfile: y" {
		t.Errorf("got %q", got)
	}
}
