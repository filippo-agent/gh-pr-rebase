package main

import (
	"context"
	"strings"
	"testing"
)

func TestSamePRIgnoresOnlyBaseSHA(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*pullRequest)
		want bool
	}{
		{"unchanged", func(p *pullRequest) {}, true},
		{"base metadata refreshed", func(p *pullRequest) { p.Base.SHA = strings.Repeat("c", 40) }, true},
		{"head changed", func(p *pullRequest) { p.Head.SHA = strings.Repeat("c", 40) }, false},
		{"base retargeted", func(p *pullRequest) { p.Base.Ref = "other" }, false},
		{"base repository changed", func(p *pullRequest) { p.Base.Repo.FullName = "other/repo" }, false},
		{"head renamed", func(p *pullRequest) { p.Head.Ref = "other" }, false},
		{"head repository changed", func(p *pullRequest) { p.Head.Repo.FullName = "other/repo" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := validPR(), validPR()
			tc.edit(&b)
			if got := samePR(a, b); got != tc.want {
				t.Fatalf("samePR = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCheckBaseTip(t *testing.T) {
	g := newTestRepo(t)
	g.write("file", "old\n")
	old := g.commit("initial")
	r := runner{ctx: context.Background(), dir: t.TempDir(), env: cleanEnv()}
	mustCommand(t, r, "git", "init", "--template=", ".")
	mustCommand(t, r, "git", "remote", "add", "base", g.dir)
	if err := checkBaseTip(r, "main", old); err != nil {
		t.Fatal(err)
	}
	g.write("file", "new\n")
	g.commit("advance base")
	if err := checkBaseTip(r, "main", old); err == nil || !strings.Contains(err.Error(), "changed during rebase") {
		t.Fatalf("checkBaseTip after movement: %v", err)
	}
	if err := checkBaseTip(r, "deleted", old); err == nil {
		t.Fatal("deleted base not rejected")
	}
}
