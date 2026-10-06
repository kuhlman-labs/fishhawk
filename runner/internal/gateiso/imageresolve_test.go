package gateiso

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRuntimeImageArgv(t *testing.T) {
	dk := Runtime{Kind: KindDocker, SocketPath: "/s/docker.sock"}
	pm := Runtime{Kind: KindPodman, SocketPath: "/s/podman.sock"}
	digest := "sha256:" + strings.Repeat("c", 64)
	b := BuildSpec{Dockerfile: "/b/Dockerfile", Context: "/b/context", Digest: digest}
	rows := []struct {
		name string
		got  func() ([]string, error)
		want []string
	}{
		{"docker inspect", func() ([]string, error) { return dk.InspectArgv("ghcr.io/o/g:1") }, []string{"docker", "--host", "unix:///s/docker.sock", "image", "inspect", "--format", imageInspectFormat, "ghcr.io/o/g:1"}},
		{"podman inspect", func() ([]string, error) { return pm.InspectArgv("ghcr.io/o/g:1") }, []string{"podman", "--url", "unix:///s/podman.sock", "image", "inspect", "--format", imageInspectFormat, "ghcr.io/o/g:1"}},
		{"docker pull", func() ([]string, error) { return dk.PullArgv("ghcr.io/o/g@" + digA) }, []string{"docker", "--host", "unix:///s/docker.sock", "pull", "ghcr.io/o/g@" + digA}},
		{"podman pull", func() ([]string, error) { return pm.PullArgv("x") }, []string{"podman", "--url", "unix:///s/podman.sock", "pull", "x"}},
		{"docker build", func() ([]string, error) { return dk.BuildArgv(b) }, []string{"docker", "--host", "unix:///s/docker.sock", "build", "--network=none", "--file", "/b/Dockerfile", "--tag", "fishhawk-gate-build:" + strings.Repeat("c", 64), "--label", "fishhawk.gate-build=1", "--label", "fishhawk.gate-build.context-digest=" + digest, "/b/context"}},
		{"podman build", func() ([]string, error) { return pm.BuildArgv(b) }, []string{"podman", "--url", "unix:///s/podman.sock", "build", "--network=none", "--file", "/b/Dockerfile", "--tag", "fishhawk-gate-build:" + strings.Repeat("c", 64), "--label", "fishhawk.gate-build=1", "--label", "fishhawk.gate-build.context-digest=" + digest, "/b/context"}},
	}
	for _, r := range rows {
		got, err := r.got()
		if err != nil || !reflect.DeepEqual(got, r.want) {
			t.Errorf("%s: got %q, %v\nwant %q", r.name, got, err, r.want)
		}
	}
	bad := []struct {
		name string
		f    func() ([]string, error)
	}{
		{"no socket", func() ([]string, error) { return Runtime{Kind: KindDocker}.PullArgv("x") }},
		{"empty ref", func() ([]string, error) { return dk.InspectArgv("") }},
		{"bad digest", func() ([]string, error) {
			return dk.BuildArgv(BuildSpec{Dockerfile: "/a", Context: "/b", Digest: "sha256:abc"})
		}},
		{"relative build paths", func() ([]string, error) {
			return dk.BuildArgv(BuildSpec{Dockerfile: "Dockerfile", Context: "/b", Digest: digest})
		}},
	}
	for _, r := range bad {
		if got, err := r.f(); err == nil || !errors.Is(err, ErrContainerSpec) {
			t.Errorf("%s: got %q, %v; want ErrContainerSpec", r.name, got, err)
		}
	}
}

func TestParseInspect(t *testing.T) {
	hexID := strings.Repeat("d", 64)
	ok := []struct {
		in   string
		want ImageInspect
	}{
		{"sha256:" + hexID + " ghcr.io/o/g@" + digA, ImageInspect{ID: "sha256:" + hexID, RepoDigests: []string{"ghcr.io/o/g@" + digA}}},
		{hexID + " docker.io/library/alpine@" + digA + " alpine@" + digB + "\n", ImageInspect{ID: "sha256:" + hexID, RepoDigests: []string{"docker.io/library/alpine@" + digA, "alpine@" + digB}}},
		{"sha256:" + hexID, ImageInspect{ID: "sha256:" + hexID, RepoDigests: []string{}}},
	}
	for _, r := range ok {
		got, err := ParseInspect(r.in)
		if err != nil || got.ID != r.want.ID || strings.Join(got.RepoDigests, ",") != strings.Join(r.want.RepoDigests, ",") {
			t.Errorf("ParseInspect(%q) = %+v, %v; want %+v", r.in, got, err, r.want)
		}
	}
	for _, in := range []string{"", "garbage", "sha256:" + hexID + " notadigest", "sha256:" + hexID + "\nsha256:" + hexID, "sha512:" + hexID} {
		if got, err := ParseInspect(in); err == nil {
			t.Errorf("ParseInspect(%q) = %+v, want error", in, got)
		}
	}
}

func TestRepoDigestFor(t *testing.T) {
	alpine, _ := ParseImageRef("alpine:3.20")
	got, ok := RepoDigestFor(alpine, []string{"ghcr.io/o/g@" + digB, "alpine@" + digA})
	if !ok || got != "docker.io/library/alpine@"+digA {
		t.Fatalf("got %q %v", got, ok)
	}
	if got, ok := RepoDigestFor(alpine, []string{"ghcr.io/o/g@" + digB, "BAD@@", "docker.io/library/alpine:3.20"}); ok {
		t.Fatalf("unmatched: got %q", got)
	}
}

func TestPullFailedReason_NamesLocalOnlyRemedy(t *testing.T) {
	ref, _ := ParseImageRef("my-local-gate:dev")
	got := PullFailedReason(ref, errors.New("pull access denied"))
	for _, want := range []string{"docker.io/library/my-local-gate:dev", "pull access denied", "must be pullable", "`dockerfile` + `context`", "FISHHAWK_GATE_IMAGE"} {
		if !strings.Contains(got, want) {
			t.Errorf("reason %q missing %q", got, want)
		}
	}
}

// buildRepo is a committed fixture: a context dir with an executable, a
// symlink, a .dockerignore, and a Dockerfile (plus its specific ignore file)
// OUTSIDE the context.
type buildRepo struct {
	dir string
	git GitFunc
}

func newBuildRepo(t *testing.T) buildRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	gitT(t, dir, "config", "commit.gpgsign", "false")
	write := func(p, c string, mode os.FileMode) {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(full, mode); err != nil {
			t.Fatal(err)
		}
	}
	write("ctx/a.txt", "alpha\n", 0o644)
	write("ctx/sub/run.sh", "#!/bin/sh\n", 0o755)
	write("ctx/.dockerignore", "ignored/\n", 0o644)
	write("ctx/ignored/x", "ignored\n", 0o644)
	write("gate/Dockerfile", "FROM alpine\nCOPY . /src\n", 0o644)
	write("gate/Dockerfile.dockerignore", "*.tmp\n", 0o644)
	write("other/z.txt", "outside the context\n", 0o644)
	if err := os.Symlink("../other/z.txt", filepath.Join(dir, "ctx", "link")); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "fixture")
	return buildRepo{dir: dir, git: ExecGit(dir)}
}

func (r buildRepo) head(t *testing.T) string { return gitT(t, r.dir, "rev-parse", "HEAD") }

func (r buildRepo) commit(t *testing.T, msg string, edit func()) string {
	t.Helper()
	edit()
	gitT(t, r.dir, "add", "-A")
	gitT(t, r.dir, "commit", "-q", "-m", msg)
	return r.head(t)
}

func (r buildRepo) source(t *testing.T, sha string) BuildSource {
	t.Helper()
	src, err := ResolveBuildSource(context.Background(), r.git, sha, "gate/Dockerfile", "ctx")
	if err != nil {
		t.Fatalf("ResolveBuildSource: %v", err)
	}
	return src
}

// TestBuildDigest_SameCommitSameDigestAcrossGateKinds pins the cache-hit
// property: the digest is a function of git objects at one commit, so the
// in-place checkout and an independent throwaway clone (the clone and
// container gate kinds) compute the same digest and the same tag, and an
// uncommitted or untracked working-tree change does not move it.
func TestBuildDigest_SameCommitSameDigestAcrossGateKinds(t *testing.T) {
	r := newBuildRepo(t)
	sha := r.head(t)
	primary := r.source(t, sha)
	clone := t.TempDir()
	rep, err := MaterializeClone(context.Background(), "", r.dir, sha, filepath.Join(clone, "c"))
	if err != nil {
		t.Fatal(err)
	}
	cloned, err := ResolveBuildSource(context.Background(), ExecGit(rep.Dest), sha, "gate/Dockerfile", "ctx")
	if err != nil {
		t.Fatal(err)
	}
	if cloned.Digest != primary.Digest || cloned.Tag() != primary.Tag() {
		t.Fatalf("clone digest %s != primary %s", cloned.Digest, primary.Digest)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "ctx", "a.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "ctx", "untracked"), []byte("u\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if again := r.source(t, sha); again.Digest != primary.Digest {
		t.Fatalf("working-tree change moved the digest: %s -> %s", primary.Digest, again.Digest)
	}
	if !strings.HasPrefix(primary.Tag(), "fishhawk-gate-build:") || len(primary.Tag()) != len("fishhawk-gate-build:")+64 {
		t.Fatalf("tag = %q", primary.Tag())
	}
}

func TestBuildDigest_MovesWithEveryInput(t *testing.T) {
	r := newBuildRepo(t)
	base := r.source(t, r.head(t)).Digest
	rows := []struct {
		name  string
		edit  func()
		moves bool
	}{
		{"mode-only change in context", func() { mustChmod(t, filepath.Join(r.dir, "ctx", "a.txt"), 0o755) }, true},
		{"context .dockerignore change", func() { mustWrite(t, filepath.Join(r.dir, "ctx", ".dockerignore"), "other/\n") }, true},
		{"ignored file change", func() { mustWrite(t, filepath.Join(r.dir, "ctx", "ignored", "x"), "changed\n") }, true},
		{"dockerfile change", func() { mustWrite(t, filepath.Join(r.dir, "gate", "Dockerfile"), "FROM alpine\n") }, true},
		{"dockerfile mode change", func() { mustChmod(t, filepath.Join(r.dir, "gate", "Dockerfile"), 0o755) }, true},
		{"dockerfile-specific ignore change", func() { mustWrite(t, filepath.Join(r.dir, "gate", "Dockerfile.dockerignore"), "*.bak\n") }, true},
		{"dockerfile-specific ignore removed", func() { mustRemove(t, filepath.Join(r.dir, "gate", "Dockerfile.dockerignore")) }, true},
		{"change outside context and dockerfile", func() { mustWrite(t, filepath.Join(r.dir, "other", "z.txt"), "moved\n") }, false},
	}
	prev := base
	for _, row := range rows {
		sha := r.commit(t, row.name, row.edit)
		got := r.source(t, sha).Digest
		if moved := got != prev; moved != row.moves {
			t.Errorf("%s: digest moved=%v, want %v (%s -> %s)", row.name, moved, row.moves, prev, got)
		}
		prev = got
	}
}

func mustWrite(t *testing.T, p, c string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustChmod(t *testing.T, p string, m os.FileMode) {
	t.Helper()
	if err := os.Chmod(p, m); err != nil {
		t.Fatal(err)
	}
}

func mustRemove(t *testing.T, p string) {
	t.Helper()
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
}

func TestResolveBuildSource_Refusals(t *testing.T) {
	r := newBuildRepo(t)
	sha := r.commit(t, "shapes", func() {
		if err := os.Symlink("gate/Dockerfile", filepath.Join(r.dir, "Dockerfile.link")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("ctx", filepath.Join(r.dir, "ctx.link")); err != nil {
			t.Fatal(err)
		}
		for _, d := range []string{"big", "ig"} {
			if err := os.MkdirAll(filepath.Join(r.dir, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		mustWrite(t, filepath.Join(r.dir, "big", "Dockerfile"), strings.Repeat("#", maxDockerfileBytes+1))
		mustWrite(t, filepath.Join(r.dir, "ig", "Dockerfile"), "FROM alpine\n")
		if err := os.Symlink("../ctx/a.txt", filepath.Join(r.dir, "ig", "Dockerfile.dockerignore")); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.WriteFile(filepath.Join(r.dir, "uncommitted.Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		name, df, cx, want string
	}{
		{"absent dockerfile", "nope/Dockerfile", "ctx", "not present in the committed tree"},
		{"uncommitted dockerfile", "uncommitted.Dockerfile", "ctx", "not present in the committed tree"},
		{"symlinked dockerfile", "Dockerfile.link", "ctx", "is a symlink"},
		{"symlinked context", "gate/Dockerfile", "ctx.link", "is a symlink"},
		{"path through symlinked dir", "ctx.link/a.txt", "ctx", "not present in the committed tree"},
		{"context is a file", "gate/Dockerfile", "ctx/a.txt", "is a blob"},
		{"dockerfile is a dir", "gate", "ctx", "is a tree"},
		{"absent context", "gate/Dockerfile", "nope", "not present"},
		{"dotdot dockerfile", "../x/Dockerfile", "ctx", "'..' segment"},
		{"dotdot context", "gate/Dockerfile", "a/../..", "'..' segment"},
		{"absolute dockerfile", "/etc/Dockerfile", "ctx", "not a repo-relative path"},
		{"oversized dockerfile", "big/Dockerfile", "ctx", "over the 1048576-byte limit"},
		{"symlinked dockerfile ignore", "ig/Dockerfile", "ctx", "dockerfile ignore file"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, err := ResolveBuildSource(context.Background(), r.git, sha, row.df, row.cx)
			if !errors.Is(err, ErrBuildSourceRefused) || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("err = %v, want ErrBuildSourceRefused containing %q", err, row.want)
			}
		})
	}
	// Operational failures are NOT refusals.
	for _, head := range []string{strings.Repeat("0", 40), "-x", ""} {
		if _, err := ResolveBuildSource(context.Background(), r.git, head, "gate/Dockerfile", "ctx"); err == nil || errors.Is(err, ErrBuildSourceRefused) {
			t.Errorf("head %q: err = %v, want a non-refusal error", head, err)
		}
	}
	// The repository root as context.
	src, err := ResolveBuildSource(context.Background(), r.git, sha, "gate/Dockerfile", ".")
	if err != nil || src.ContextTree != gitT(t, r.dir, "rev-parse", sha+"^{tree}") {
		t.Fatalf("root context: %+v, %v", src, err)
	}
	if src.IgnoreBlob == "" || src.DockerfileMode != "100644" || src.Commit != sha {
		t.Fatalf("root context source = %+v", src)
	}
}

func TestMaterializeBuildContext_CommittedTreeOnly(t *testing.T) {
	r := newBuildRepo(t)
	sha := r.head(t)
	mustWrite(t, filepath.Join(r.dir, "ctx", "a.txt"), "dirty\n")
	mustWrite(t, filepath.Join(r.dir, "ctx", "untracked"), "u\n")
	mustWrite(t, filepath.Join(r.dir, "gate", "Dockerfile"), "FROM evil\n")
	src := r.source(t, sha)
	dest := filepath.Join(t.TempDir(), "build")
	lay, err := MaterializeBuildContext(context.Background(), r.git, src, dest, ContextLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if lay.Dockerfile != filepath.Join(dest, "Dockerfile") || lay.Context != filepath.Join(dest, "context") {
		t.Fatalf("layout = %+v", lay)
	}
	readEq := func(p, want string) {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil || string(b) != want {
			t.Errorf("%s = %q, %v; want %q", p, b, err, want)
		}
	}
	readEq(filepath.Join(lay.Context, "a.txt"), "alpha\n")
	readEq(filepath.Join(lay.Context, ".dockerignore"), "ignored/\n")
	readEq(filepath.Join(lay.Context, "ignored", "x"), "ignored\n")
	readEq(lay.Dockerfile, "FROM alpine\nCOPY . /src\n")
	readEq(lay.Dockerfile+".dockerignore", "*.tmp\n")
	if _, err := os.Lstat(filepath.Join(lay.Context, "untracked")); !os.IsNotExist(err) {
		t.Errorf("untracked file materialized: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(lay.Context, "sub", "run.sh")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("run.sh mode = %v, %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Join(lay.Context, "a.txt")); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("a.txt mode = %v, %v", fi, err)
	}
	if target, err := os.Readlink(filepath.Join(lay.Context, "link")); err != nil || target != "../other/z.txt" {
		t.Errorf("link = %q, %v", target, err)
	}
	// The Dockerfile bytes the scan screens are the committed ones.
	b, err := ReadDockerfile(context.Background(), r.git, src)
	if err != nil || string(b) != "FROM alpine\nCOPY . /src\n" {
		t.Errorf("ReadDockerfile = %q, %v", b, err)
	}
	// A non-empty destination is refused.
	if _, err := MaterializeBuildContext(context.Background(), r.git, src, dest, ContextLimits{}); err == nil {
		t.Error("non-empty destination accepted")
	}
	if _, err := MaterializeBuildContext(context.Background(), r.git, src, "relative", ContextLimits{}); err == nil {
		t.Error("relative destination accepted")
	}
}

func TestMaterializeBuildContext_Limits(t *testing.T) {
	r := newBuildRepo(t)
	src := r.source(t, r.head(t))
	for name, lim := range map[string]ContextLimits{
		"entries": {MaxEntries: 2, MaxBytes: 1 << 30},
		"bytes":   {MaxEntries: 100, MaxBytes: 8},
	} {
		dest := filepath.Join(t.TempDir(), "b")
		_, err := MaterializeBuildContext(context.Background(), r.git, src, dest, lim)
		if !errors.Is(err, ErrContextTooLarge) {
			t.Errorf("%s: err = %v, want ErrContextTooLarge", name, err)
		}
		if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
			t.Errorf("%s: destination written before the bound check: %v", name, statErr)
		}
	}
}

func TestMaterializeBuildContext_SymlinkedIgnoreFileRefused(t *testing.T) {
	r := newBuildRepo(t)
	sha := r.commit(t, "link ignore", func() {
		mustRemove(t, filepath.Join(r.dir, "ctx", ".dockerignore"))
		if err := os.Symlink("/etc/hosts", filepath.Join(r.dir, "ctx", ".dockerignore")); err != nil {
			t.Fatal(err)
		}
	})
	_, err := MaterializeBuildContext(context.Background(), r.git, r.source(t, sha), filepath.Join(t.TempDir(), "b"), ContextLimits{})
	if !errors.Is(err, ErrBuildSourceRefused) || !strings.Contains(err.Error(), "is a symlink") {
		t.Fatalf("err = %v", err)
	}
}

func TestTreeWriter_NeverWritesThroughASymlink(t *testing.T) {
	root := t.TempDir()
	w := &treeWriter{root: root, dirs: map[string]bool{".": true}}
	outside := t.TempDir()
	if err := w.write(gitTreeEntry{mode: "120000", path: "a"}, []byte(outside)); err != nil {
		t.Fatal(err)
	}
	err := w.write(gitTreeEntry{mode: "100644", path: "a/b"}, []byte("x"))
	if !errors.Is(err, ErrBuildSourceRefused) {
		t.Fatalf("write through symlink: err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outside, "b")); !os.IsNotExist(statErr) {
		t.Fatalf("file written through the symlink: %v", statErr)
	}
	// A duplicate file entry is never overwritten.
	if err := w.write(gitTreeEntry{mode: "100644", path: "f"}, []byte("1")); err != nil {
		t.Fatal(err)
	}
	if err := w.write(gitTreeEntry{mode: "100644", path: "f"}, []byte("2")); err == nil {
		t.Fatal("duplicate entry overwrote a file")
	}
}

func TestCheckTreePath(t *testing.T) {
	for _, p := range []string{"", "/abs", "a/../b", "..", ".", "a//b", "./a", "a/./b"} {
		if err := checkTreePath(p); !errors.Is(err, ErrBuildSourceRefused) {
			t.Errorf("checkTreePath(%q) = %v", p, err)
		}
	}
	for _, p := range []string{"a", "a/b", ".hidden/x"} {
		if err := checkTreePath(p); err != nil {
			t.Errorf("checkTreePath(%q) = %v", p, err)
		}
	}
}

func TestParseTreeEntries_Malformed(t *testing.T) {
	for _, in := range []string{"100644 blob abc 1\tx\x00", "100644 blob " + strings.Repeat("a", 40) + " NaN\tx\x00", "no tab here\x00"} {
		if _, err := parseTreeEntries(in); err == nil {
			t.Errorf("parseTreeEntries(%q) accepted", in)
		}
	}
}

func TestConsumeBatch_Malformed(t *testing.T) {
	oid := strings.Repeat("a", 40)
	e := []gitTreeEntry{{oid: oid, size: 3, path: "x"}}
	rows := map[string]string{
		"wrong oid":          strings.Repeat("b", 40) + " blob 3\nabc\n",
		"wrong type":         oid + " tree 3\nabc\n",
		"size mismatch":      oid + " blob 4\nabcd\n",
		"missing terminator": oid + " blob 3\nabcX",
		"truncated":          oid + " blob 3\nab",
		"no header":          "",
	}
	for name, in := range rows {
		err := consumeBatch(bufio.NewReader(strings.NewReader(in)), e, func(gitTreeEntry, []byte) error { return nil })
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestExecGit_ReportsStderr(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	err := ExecGit(t.TempDir())(context.Background(), nil, &strings.Builder{}, "rev-parse", "HEAD")
	if err == nil || !strings.Contains(err.Error(), "git rev-parse HEAD") {
		t.Fatalf("err = %v", err)
	}
}
