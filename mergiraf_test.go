package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const fakeMergiraf = `#!/bin/sh
printf 'CALL' >>"$MERGIRAF_LOG"
for arg do
  printf '\t[%s]' "$arg" >>"$MERGIRAF_LOG"
done
printf '\n' >>"$MERGIRAF_LOG"

if [ "$1" = languages ] && [ "$2" = --gitattributes ]; then
  if [ "$MERGIRAF_MODE" = language-error ]; then
    echo "language discovery failed" >&2
    exit 41
  fi
  printf '*.go merge=mergiraf\n'
  exit 0
fi
if [ "$1" = merge ] && [ "$2" = --git ]; then
  case "$MERGIRAF_MODE" in
    success)
      printf 'package p\n\nconst Value = "merged"\n' >"$4"
      exit 0
      ;;
    unresolved)
      printf '<<<<<<< ours\nunresolved\n=======\nstill unresolved\n>>>>>>> theirs\n' >"$4"
      exit 1
      ;;
    error)
      echo "synthetic mergiraf failure" >&2
      exit 42
      ;;
  esac
fi
echo "unexpected fake mergiraf invocation" >&2
exit 64
`

func installFakeMergiraf(t *testing.T, mode string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX fake mergiraf")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "fake tools' bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(bin, "mergiraf"), fakeMergiraf)
	log := filepath.Join(root, "calls.log")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MERGIRAF_LOG", log)
	t.Setenv("MERGIRAF_MODE", mode)
	return log
}

func conflictingRepo(t *testing.T, path string, withEarlierCommit bool) (*testRepo, string, string) {
	t.Helper()
	g := newTestRepo(t)
	g.write(path, "package p\n\nconst Value = \"initial\"\n")
	initial := g.commit("initial")

	mustCommand(t, g.r, "git", "checkout", "-b", "feature", initial)
	if withEarlierCommit {
		g.write("feature setup.txt", "must survive both attempts\n")
		g.commit("feature setup")
	}
	g.write(path, "package p\n\nconst Value = \"feature\"\n")
	head := g.commit("feature conflict")

	mustCommand(t, g.r, "git", "checkout", "main")
	g.write(path, "package p\n\nconst Value = \"base\"\n")
	base := g.commit("base conflict")
	return g, base, head
}

func fakeCalls(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestRebaseRetriesWithMergirafFromOriginalHead(t *testing.T) {
	log := installFakeMergiraf(t, "success")
	conflictPath := filepath.Join("dir with space", "it's.go")
	g, base, head := conflictingRepo(t, conflictPath, true)

	var out bytes.Buffer
	got, err := rebase(g.r, base, head, &out)
	if err != nil {
		t.Fatalf("rebase() error = %v\noutput:\n%s", err, out.String())
	}
	if got == head {
		t.Fatal("rebase did not rewrite the feature history")
	}
	if !strings.Contains(out.String(), "retrying the rebase with Mergiraf") {
		t.Fatalf("rebase output = %q, want fallback notice", out.String())
	}
	if contents := mustCommand(t, g.r, "git", "show", got+":"+conflictPath); contents != "package p\n\nconst Value = \"merged\"" {
		t.Fatalf("merged file = %q", contents)
	}
	if subjects := mustCommand(t, g.r, "git", "log", "--reverse", "--format=%s", base+".."+got); subjects != "feature setup\nfeature conflict" {
		t.Fatalf("rebased subjects = %q, want both original commits", subjects)
	}
	if setup := mustCommand(t, g.r, "git", "show", got+"^:feature setup.txt"); setup != "must survive both attempts" {
		t.Fatalf("earlier commit contents = %q", setup)
	}

	calls := fakeCalls(t, log)
	if len(calls) != 2 {
		t.Fatalf("mergiraf calls = %q, want one discovery and one merge", calls)
	}
	if calls[0] != "CALL\t[languages]\t[--gitattributes]" {
		t.Fatalf("discovery call = %q", calls[0])
	}
	wantPathArgs := "\t[-p]\t[" + conflictPath + "]\t[-l]"
	if !strings.Contains(calls[1], wantPathArgs) {
		t.Fatalf("merge call = %q, want shell-safe path args %q", calls[1], wantPathArgs)
	}
	attrs, err := os.ReadFile(filepath.Join(g.dir, ".git", "info", "attributes"))
	if err != nil {
		t.Fatal(err)
	}
	if string(attrs) != "*.go merge=mergiraf\n" {
		t.Fatalf("info attributes = %q", attrs)
	}
}

func TestRebaseWithRealMergiraf(t *testing.T) {
	if _, err := exec.LookPath("mergiraf"); err != nil {
		t.Skip("mergiraf is not installed on PATH")
	}
	g := newTestRepo(t)
	path := "person.go"
	g.write(path, "package p\n\ntype Person struct {\n\tName string\n}\n")
	initial := g.commit("initial")

	mustCommand(t, g.r, "git", "checkout", "-b", "feature", initial)
	g.write(path, "package p\n\ntype Person struct {\n\tName string\n\tAge int\n}\n")
	head := g.commit("add age")

	mustCommand(t, g.r, "git", "checkout", "main")
	g.write(path, "package p\n\ntype Person struct {\n\tName string\n\tEmail string\n}\n")
	base := g.commit("add email")

	var out bytes.Buffer
	got, err := rebase(g.r, base, head, &out)
	if err != nil {
		t.Fatalf("real Mergiraf rebase error = %v\noutput:\n%s", err, out.String())
	}
	contents := mustCommand(t, g.r, "git", "show", got+":"+path)
	for _, field := range []string{"Name string", "Age int", "Email string"} {
		if !strings.Contains(contents, field) {
			t.Fatalf("merged Go source missing %q:\n%s", field, contents)
		}
	}
	if strings.Contains(contents, "<<<<<<<") {
		t.Fatalf("merged Go source contains conflict markers:\n%s", contents)
	}
	if !strings.Contains(out.String(), "retrying the rebase with Mergiraf") {
		t.Fatalf("rebase output = %q, want fallback notice", out.String())
	}
}

func TestRebaseWithRealMergirafLeavesConflictUnresolved(t *testing.T) {
	if _, err := exec.LookPath("mergiraf"); err != nil {
		t.Skip("mergiraf is not installed on PATH")
	}
	g, base, head := conflictingRepo(t, "value.go", false)
	if got, err := rebase(g.r, base, head, io.Discard); err == nil {
		t.Fatalf("rebase() = %q, want unresolved semantic conflict", got)
	} else if !strings.Contains(err.Error(), "rebase with Mergiraf did not succeed") {
		t.Fatalf("rebase() error = %v, want failed fallback", err)
	}
	if unmerged := mustCommand(t, g.r, "git", "ls-files", "--unmerged"); unmerged == "" {
		t.Fatal("real Mergiraf failure left no unmerged index entries")
	}
}

func TestRebaseDoesNotCallMergirafWhenClean(t *testing.T) {
	log := installFakeMergiraf(t, "error")
	g := newTestRepo(t)
	g.write("common", "initial\n")
	initial := g.commit("initial")
	mustCommand(t, g.r, "git", "checkout", "-b", "feature", initial)
	g.write("feature", "feature\n")
	head := g.commit("feature")
	mustCommand(t, g.r, "git", "checkout", "main")
	g.write("base", "base\n")
	base := g.commit("base")

	if _, err := rebase(g.r, base, head, io.Discard); err != nil {
		t.Fatalf("clean rebase error = %v", err)
	}
	if data, err := os.ReadFile(log); err == nil {
		t.Fatalf("mergiraf unexpectedly called: %q", data)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestRebaseMergirafFailureDoesNotSucceedOrPush(t *testing.T) {
	for _, mode := range []string{"unresolved", "error", "language-error"} {
		t.Run(mode, func(t *testing.T) {
			log := installFakeMergiraf(t, mode)
			g, base, head := conflictingRepo(t, "conflict.go", false)

			remote := filepath.Join(t.TempDir(), "remote.git")
			outside := runner{ctx: context.Background(), dir: t.TempDir(), env: cleanEnv()}
			mustCommand(t, outside, "git", "init", "--bare", "--template=", remote)
			mustCommand(t, g.r, "git", "remote", "add", "origin", remote)
			mustCommand(t, g.r, "git", "push", "origin", head+":refs/heads/feature")

			if got, err := rebase(g.r, base, head, io.Discard); err == nil {
				t.Fatalf("rebase() = %q, want Mergiraf failure", got)
			} else if !strings.Contains(err.Error(), "nothing was pushed") {
				t.Fatalf("rebase() error = %v, want push safety message", err)
			}
			if remoteHead := mustCommand(t, outside, "git", "--git-dir="+remote, "rev-parse", "refs/heads/feature"); remoteHead != head {
				t.Fatalf("remote head = %q, want unchanged %q", remoteHead, head)
			}

			calls := fakeCalls(t, log)
			wantCalls := 2
			if mode == "language-error" {
				wantCalls = 1
			}
			if len(calls) != wantCalls {
				t.Fatalf("mergiraf calls = %q, want %d (no extra retry)", calls, wantCalls)
			}
			if mode != "language-error" && !strings.HasPrefix(calls[1], "CALL\t[merge]\t[--git]") {
				t.Fatalf("merge call = %q", calls[1])
			}
		})
	}
}
