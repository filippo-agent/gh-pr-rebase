package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const fakeTargetGH = `#!/bin/sh
printf 'args' >>"$GH_TEST_LOG"
for arg do printf '\t%s' "$arg" >>"$GH_TEST_LOG"; done
printf '\nGH_PROMPT_DISABLED=%s\nGH_REPO=%s\nGIT_CONFIG_GLOBAL=%s\nGH_HOST=%s\n' \
  "$GH_PROMPT_DISABLED" "$GH_REPO" "$GIT_CONFIG_GLOBAL" "$GH_HOST" >>"$GH_TEST_LOG"
if [ -n "$GH_TEST_ERROR" ]; then
  printf '%s\n' "$GH_TEST_ERROR" >&2
  exit 7
fi
printf '%s\n' "$GH_TEST_OUTPUT"
`

func installTargetGH(t *testing.T, output string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX fake gh")
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "gh.log")
	writeExecutable(t, filepath.Join(bin, "gh"), fakeTargetGH)
	t.Setenv("PATH", bin)
	t.Setenv("GH_TEST_LOG", log)
	t.Setenv("GH_TEST_OUTPUT", output)
	t.Setenv("GH_TEST_ERROR", "")
	t.Setenv("GH_HOST", "")
	return log
}

func readTargetGHLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestResolveTargetThroughGH(t *testing.T) {
	t.Setenv("GH_REPO", "inherited/repo")
	t.Setenv("GIT_CONFIG_GLOBAL", "/tmp/inherited-git-config")
	tests := []struct {
		name, input, host, output, args string
		hostExplicit                    bool
		want                            target
	}{
		{"number normalization and enterprise host", "#007", "github.com", `{"url":"https://ghe.example/owner/repo/pull/7"}`, "args\tpr\tview\t7\t--json\turl", false, target{"ghe.example", "owner/repo", "7"}},
		{"current branch", "", "github.com", `{"url":"https://github.com/owner/repo/pull/12"}`, "args\tpr\tview\t--json\turl", false, target{"github.com", "owner/repo", "12"}},
		{"explicit host override", "9", "chosen.example", `{"url":"https://discovered.example/owner/repo/pull/9"}`, "args\tpr\tview\t9\t--json\turl", true, target{"chosen.example", "owner/repo", "9"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := installTargetGH(t, tt.output)
			got, err := resolveTarget(context.Background(), tt.input, tt.host, tt.hostExplicit)
			if err != nil {
				t.Fatalf("resolveTarget() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("resolveTarget() = %#v, want %#v", got, tt.want)
			}
			contents := readTargetGHLog(t, log)
			for _, want := range []string{tt.args, "GH_PROMPT_DISABLED=1", "GH_REPO=inherited/repo", "GIT_CONFIG_GLOBAL=/tmp/inherited-git-config"} {
				if !strings.Contains(contents, want) {
					t.Fatalf("fake gh log missing %q:\n%s", want, contents)
				}
			}
			if tt.hostExplicit && !strings.Contains(contents, "GH_HOST="+tt.host) {
				t.Fatalf("fake gh log missing explicit host:\n%s", contents)
			}
		})
	}
}

func TestResolveTargetExplicitInputsDoNotInvokeGH(t *testing.T) {
	tests := []struct {
		name, input, host string
		hostExplicit      bool
		want              target
	}{
		{"shorthand", "owner/repo#0042", "ghe.example", true, target{"ghe.example", "owner/repo", "42"}},
		{"URL keeps its host", "https://url.example/owner/repo/pull/8", "override.example", true, target{"url.example", "owner/repo", "8"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := installTargetGH(t, "")
			got, err := resolveTarget(context.Background(), tt.input, tt.host, tt.hostExplicit)
			if err != nil {
				t.Fatalf("resolveTarget() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("resolveTarget() = %#v, want %#v", got, tt.want)
			}
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatalf("explicit target invoked gh (stat error %v)", err)
			}
		})
	}
}

func TestResolveTargetRejectsBadInputAndGHOutput(t *testing.T) {
	tests := []struct {
		name, input, output, want string
		invokesGH                 bool
	}{
		{"invalid input", "not-a-target", "", "expected NUMBER", false},
		{"zero", "#0", "", "invalid pull request number", false},
		{"malformed JSON", "3", `{`, "invalid gh pr view response", true},
		{"missing URL", "3", `{}`, "invalid pull request URL from gh", true},
		{"malformed URL", "3", `{"url":"not-a-url"}`, "invalid pull request URL from gh", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := installTargetGH(t, tt.output)
			_, err := resolveTarget(context.Background(), tt.input, "github.com", false)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("resolveTarget() error = %v, want containing %q", err, tt.want)
			}
			_, statErr := os.Stat(log)
			if tt.invokesGH && statErr != nil {
				t.Fatalf("gh was not invoked: %v", statErr)
			}
			if !tt.invokesGH && !os.IsNotExist(statErr) {
				t.Fatalf("invalid input invoked gh (stat error %v)", statErr)
			}
		})
	}
}

func TestResolveTargetNonGitHubRepositoryError(t *testing.T) {
	log := installTargetGH(t, "")
	t.Setenv("GH_TEST_ERROR", "no git remotes found")
	_, err := resolveTarget(context.Background(), "", "github.com", false)
	if err == nil || !strings.Contains(err.Error(), "cannot resolve pull request in the current GitHub repository") || !strings.Contains(err.Error(), "use a PR URL or OWNER/REPO#NUMBER") {
		t.Fatalf("resolveTarget() error = %v, want clear repository-selection guidance", err)
	}
	if !strings.Contains(readTargetGHLog(t, log), "args\tpr\tview\t--json\turl") {
		t.Fatal("fake gh did not receive current-branch lookup")
	}
}
