package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	testName  = "Test User"
	testEmail = "test@example.invalid"
)

func TestParseTarget(t *testing.T) {
	tests := []struct {
		name  string
		input string
		host  string
		want  target
	}{
		{
			name:  "shorthand",
			input: "owner/repo#42",
			host:  "github.example.com",
			want:  target{Host: "github.example.com", Repo: "owner/repo", Number: "42"},
		},
		{
			name:  "URL overrides host and trims slashes",
			input: "https://github.com/owner/repo/pull/007/",
			host:  "ignored.example.com",
			want:  target{Host: "github.com", Repo: "owner/repo", Number: "7"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTarget(tt.input, tt.host)
			if err != nil {
				t.Fatalf("parseTarget() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("parseTarget() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseTargetRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name, input, host string
	}{
		{"missing number", "owner/repo", "github.com"},
		{"empty number", "owner/repo#", "github.com"},
		{"zero number", "owner/repo#0", "github.com"},
		{"negative number", "owner/repo#-1", "github.com"},
		{"non-numeric number", "owner/repo#abc", "github.com"},
		{"extra hash", "owner/repo#1#2", "github.com"},
		{"invalid repository", "owner/repo/extra#1", "github.com"},
		{"invalid shorthand host", "owner/repo#1", "bad:host"},
		{"HTTP URL", "http://github.com/owner/repo/pull/1", "github.com"},
		{"wrong URL path", "https://github.com/owner/repo/issues/1", "github.com"},
		{"short URL path", "https://github.com/owner/repo/pull", "github.com"},
		{"URL query", "https://github.com/owner/repo/pull/1?x=y", "github.com"},
		{"URL fragment", "https://github.com/owner/repo/pull/1#x", "github.com"},
		{"URL credentials", "https://user@github.com/owner/repo/pull/1", "github.com"},
		{"URL port", "https://github.com:443/owner/repo/pull/1", "github.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := parseTarget(tt.input, tt.host); err == nil {
				t.Fatalf("parseTarget() = %#v, want an error", got)
			}
		})
	}
}

func validPR() pullRequest {
	return pullRequest{
		State:               "open",
		MaintainerCanModify: true,
		Head: branch{
			Ref:  "feature",
			SHA:  strings.Repeat("a", 40),
			Repo: &repository{FullName: "owner/fork"},
		},
		Base: branch{
			Ref:  "main",
			SHA:  strings.Repeat("b", 40),
			Repo: &repository{FullName: "owner/upstream"},
		},
	}
}

func TestPullRequestValidate(t *testing.T) {
	if err := validPR().validate(); err != nil {
		t.Fatalf("valid PR rejected: %v", err)
	}

	tests := []struct {
		name string
		edit func(*pullRequest)
		want string
	}{
		{"closed", func(p *pullRequest) { p.State = "closed" }, "not open"},
		{"merged", func(p *pullRequest) { p.Merged = true }, "not open"},
		{"deleted head repository", func(p *pullRequest) { p.Head.Repo = nil }, "repository was deleted"},
		{"deleted base repository", func(p *pullRequest) { p.Base.Repo = nil }, "repository was deleted"},
		{"invalid head repository", func(p *pullRequest) { p.Head.Repo.FullName = "owner/repo/extra" }, "invalid repository metadata"},
		{"invalid base repository", func(p *pullRequest) { p.Base.Repo.FullName = "owner repo" }, "invalid repository metadata"},
		{"short head SHA", func(p *pullRequest) { p.Head.SHA = "abc" }, "invalid repository metadata"},
		{"uppercase base SHA", func(p *pullRequest) { p.Base.SHA = strings.Repeat("A", 40) }, "invalid repository metadata"},
		{"fork disallows edits", func(p *pullRequest) { p.MaintainerCanModify = false }, "does not allow edits"},
		{"same branch", func(p *pullRequest) { p.Head.Repo.FullName = p.Base.Repo.FullName; p.Head.Ref = p.Base.Ref }, "same branch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validPR()
			tt.edit(&p)
			err := p.validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validate() error = %v, want error containing %q", err, tt.want)
			}
		})
	}

	t.Run("same repository different branches needs no maintainer permission", func(t *testing.T) {
		p := validPR()
		p.Head.Repo.FullName = p.Base.Repo.FullName
		p.MaintainerCanModify = false
		if err := p.validate(); err != nil {
			t.Fatalf("validate() error = %v", err)
		}
	})
}

type testRepo struct {
	t   *testing.T
	dir string
	r   runner
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	dir := t.TempDir()
	r := runner{ctx: context.Background(), dir: dir, env: cleanEnv()}
	mustCommand(t, r, "git", "init", "--template=", "-b", "main", ".")
	mustCommand(t, r, "git", "config", "user.name", testName)
	mustCommand(t, r, "git", "config", "user.email", testEmail)
	mustCommand(t, r, "git", "config", "commit.gpgSign", "false")
	mustCommand(t, r, "git", "config", "core.hooksPath", os.DevNull)
	return &testRepo{t: t, dir: dir, r: r}
}

func mustCommand(t *testing.T, r runner, name string, args ...string) string {
	t.Helper()
	out, err := r.command(name, args...)
	if err != nil {
		t.Fatalf("command failed: %v", err)
	}
	return out
}

func (g *testRepo) write(path, contents string) {
	g.t.Helper()
	full := filepath.Join(g.dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		g.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		g.t.Fatal(err)
	}
}

func (g *testRepo) commit(message string) string {
	g.t.Helper()
	mustCommand(g.t, g.r, "git", "add", "-A")
	mustCommand(g.t, g.r, "git", "commit", "-m", message)
	return mustCommand(g.t, g.r, "git", "rev-parse", "HEAD")
}

func TestRebaseSuccess(t *testing.T) {
	g := newTestRepo(t)
	g.write("common", "initial\n")
	initial := g.commit("initial")

	mustCommand(t, g.r, "git", "checkout", "-b", "feature", initial)
	g.write("feature", "feature\n")
	head := g.commit("feature work")

	mustCommand(t, g.r, "git", "checkout", "main")
	g.write("base", "base\n")
	base := g.commit("base work")

	got, err := rebase(g.r, base, head, io.Discard)
	if err != nil {
		t.Fatalf("rebase() error = %v", err)
	}
	if got == head {
		t.Fatal("rebase did not rewrite the feature commit")
	}
	if parent := mustCommand(t, g.r, "git", "show", "-s", "--format=%P", got); parent != base {
		t.Fatalf("rebased commit parent = %q, want %q", parent, base)
	}
	if message := mustCommand(t, g.r, "git", "show", "-s", "--format=%s", got); message != "feature work" {
		t.Fatalf("rebased commit message = %q", message)
	}
}

func TestRebaseNoop(t *testing.T) {
	g := newTestRepo(t)
	g.write("common", "initial\n")
	g.commit("initial")
	g.write("base", "base\n")
	base := g.commit("base work")
	mustCommand(t, g.r, "git", "checkout", "-b", "feature")
	g.write("feature", "feature\n")
	head := g.commit("feature work")

	got, err := rebase(g.r, base, head, io.Discard)
	if err != nil {
		t.Fatalf("rebase() error = %v", err)
	}
	if got != head {
		t.Fatalf("rebase() = %q, want unchanged head %q", got, head)
	}
}

func TestRebaseConflict(t *testing.T) {
	g := newTestRepo(t)
	g.write("conflict", "initial\n")
	initial := g.commit("initial")

	mustCommand(t, g.r, "git", "checkout", "-b", "feature", initial)
	g.write("conflict", "feature\n")
	head := g.commit("feature edit")

	mustCommand(t, g.r, "git", "checkout", "main")
	g.write("conflict", "base\n")
	base := g.commit("base edit")

	_, err := rebase(g.r, base, head, io.Discard)
	if err == nil {
		t.Fatal("rebase() succeeded, want a conflict")
	}
	if !strings.Contains(err.Error(), "nothing was pushed") {
		t.Fatalf("rebase() error = %v, want safety explanation", err)
	}
}

func TestRebasePreservesMergeTopology(t *testing.T) {
	g := newTestRepo(t)
	g.write("common", "initial\n")
	initial := g.commit("initial")

	mustCommand(t, g.r, "git", "checkout", "-b", "feature", initial)
	g.write("feature-one", "one\n")
	g.commit("feature one")
	mustCommand(t, g.r, "git", "branch", "side")
	g.write("feature-two", "two\n")
	g.commit("feature two")
	mustCommand(t, g.r, "git", "checkout", "side")
	g.write("side", "side\n")
	g.commit("side work")
	mustCommand(t, g.r, "git", "checkout", "feature")
	mustCommand(t, g.r, "git", "merge", "--no-ff", "side", "-m", "merge side")
	head := mustCommand(t, g.r, "git", "rev-parse", "HEAD")

	mustCommand(t, g.r, "git", "checkout", "main")
	g.write("base", "base\n")
	base := g.commit("base work")

	got, err := rebase(g.r, base, head, io.Discard)
	if err != nil {
		t.Fatalf("rebase() error = %v", err)
	}
	if got == head {
		t.Fatal("rebase did not rewrite the merge history")
	}
	parents := strings.Fields(mustCommand(t, g.r, "git", "show", "-s", "--format=%P", got))
	if len(parents) != 2 {
		t.Fatalf("rebased tip has %d parents (%v), want a merge commit", len(parents), parents)
	}
	if _, err := g.r.command("git", "merge-base", "--is-ancestor", base, got); err != nil {
		t.Fatalf("new base is not an ancestor of rebased merge: %v", err)
	}
	if subject := mustCommand(t, g.r, "git", "show", "-s", "--format=%s", got); subject != "merge side" {
		t.Fatalf("merge subject = %q", subject)
	}
}

func TestPushRejectsConcurrentUpdateWithExplicitLease(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	baseRunner := runner{ctx: context.Background(), dir: root, env: cleanEnv()}
	mustCommand(t, baseRunner, "git", "init", "--bare", "--template=", remote)

	publisher := newTestRepo(t)
	publisher.write("file", "old\n")
	old := publisher.commit("old")
	mustCommand(t, publisher.r, "git", "remote", "add", "origin", remote)
	mustCommand(t, publisher.r, "git", "push", "origin", old+":refs/heads/feature")
	publisher.write("publisher", "publisher\n")
	newSHA := publisher.commit("publisher update")

	concurrent := newTestRepo(t)
	mustCommand(t, concurrent.r, "git", "remote", "add", "origin", remote)
	mustCommand(t, concurrent.r, "git", "fetch", "origin", "refs/heads/feature")
	mustCommand(t, concurrent.r, "git", "checkout", "-b", "feature", "FETCH_HEAD")
	concurrent.write("concurrent", "concurrent\n")
	concurrentSHA := concurrent.commit("concurrent update")
	mustCommand(t, concurrent.r, "git", "push", "origin", "HEAD:refs/heads/feature")

	err := push(publisher.r, remote, "feature", old, newSHA)
	if err == nil {
		t.Fatal("push() succeeded despite a concurrent remote update")
	}
	if !strings.Contains(err.Error(), "force-with-lease") {
		t.Fatalf("push() error does not show use of an explicit lease: %v", err)
	}
	got := mustCommand(t, baseRunner, "git", "--git-dir="+remote, "rev-parse", "refs/heads/feature")
	if got != concurrentSHA {
		t.Fatalf("remote branch = %q, want concurrent commit %q", got, concurrentSHA)
	}
}

func TestRunDryRunWithLocalRemote(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX test command wrappers")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}

	work := newTestRepo(t)
	work.write("common", "initial\n")
	initial := work.commit("initial")
	mustCommand(t, work.r, "git", "checkout", "-b", "feature", initial)
	work.write("feature", "feature\n")
	head := work.commit("feature work")
	mustCommand(t, work.r, "git", "checkout", "main")
	work.write("base", "base\n")
	base := work.commit("base work")

	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	r := runner{ctx: context.Background(), dir: root, env: cleanEnv()}
	mustCommand(t, r, "git", "init", "--bare", "--template=", remote)
	mustCommand(t, work.r, "git", "remote", "add", "test-origin", remote)
	mustCommand(t, work.r, "git", "push", "test-origin", base+":refs/heads/main", head+":refs/heads/feature")

	pr := pullRequest{
		State: "open",
		Head:  branch{Ref: "feature", SHA: head, Repo: &repository{FullName: "owner/repo"}},
		Base:  branch{Ref: "main", SHA: base, Repo: &repository{FullName: "owner/repo"}},
	}
	prJSON, err := json.Marshal(pr)
	if err != nil {
		t.Fatal(err)
	}
	prFile := filepath.Join(root, "pr.json")
	if err := os.WriteFile(prFile, prJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	userFile := filepath.Join(root, "user.json")
	if err := os.WriteFile(userFile, []byte(`{"Login":"tester","Name":"Test User","ID":123}`), 0o644); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	gitWrapper := `#!/bin/sh
if [ "$1" = "remote" ] && [ "$2" = "add" ]; then
  exec "$REAL_GIT" remote add "$3" "$TEST_REMOTE"
fi
if [ "$1" = "fetch" ]; then
  exec "$REAL_GIT" -c protocol.file.allow=always "$@"
fi
exec "$REAL_GIT" "$@"
`
	ghWrapper := `#!/bin/sh
case "$*" in
  *" user") cat "$TEST_USER_JSON" ;;
  *" repos/"*) cat "$TEST_PR_JSON" ;;
  *) echo "unexpected gh invocation: $*" >&2; exit 2 ;;
esac
`
	writeExecutable(t, filepath.Join(bin, "git"), gitWrapper)
	writeExecutable(t, filepath.Join(bin, "gh"), ghWrapper)
	t.Setenv("REAL_GIT", realGit)
	t.Setenv("TEST_REMOTE", remote)
	t.Setenv("TEST_PR_JSON", prFile)
	t.Setenv("TEST_USER_JSON", userFile)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var out bytes.Buffer
	if err := run(context.Background(), []string{"--dry-run", "https://example.test/owner/repo/pull/1"}, &out); err != nil {
		t.Fatalf("run() error = %v\noutput:\n%s", err, out.String())
	}
	if got := out.String(); !strings.Contains(got, "Dry run succeeded:") || !strings.Contains(got, "nothing pushed") {
		t.Fatalf("run() output = %q", got)
	}
	remoteHead := mustCommand(t, r, realGit, "--git-dir="+remote, "rev-parse", "refs/heads/feature")
	if remoteHead != head {
		t.Fatalf("dry run changed remote feature from %q to %q", head, remoteHead)
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerCommandIncludesOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	r := runner{ctx: context.Background(), dir: t.TempDir(), env: cleanEnv()}
	_, err := r.command("sh", "-c", "echo diagnostic; exit 7")
	if err == nil || !strings.Contains(err.Error(), "diagnostic") {
		t.Fatalf("command error = %v, want captured output", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("command error does not wrap exec.ExitError: %v", err)
	}
}
