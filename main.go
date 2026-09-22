// gh-pr-rebase rebases a GitHub pull request using GitHub CLI authentication.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type repository struct {
	FullName string `json:"full_name"`
}
type branch struct {
	Ref  string      `json:"ref"`
	SHA  string      `json:"sha"`
	Repo *repository `json:"repo"`
}
type pullRequest struct {
	State               string `json:"state"`
	Merged              bool   `json:"merged"`
	MaintainerCanModify bool   `json:"maintainer_can_modify"`
	Head                branch `json:"head"`
	Base                branch `json:"base"`
}
type target struct{ Host, Repo, Number string }

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var hostPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func parseTarget(input, host string) (target, error) {
	var t target
	if strings.HasPrefix(input, "https://") {
		u, err := url.Parse(input)
		if err != nil {
			return t, err
		}
		if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Port() != "" {
			return t, errors.New("expected a plain HTTPS pull request URL")
		}
		p := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(p) != 4 || p[2] != "pull" {
			return t, errors.New("expected https://HOST/OWNER/REPO/pull/NUMBER")
		}
		t = target{u.Host, p[0] + "/" + p[1], p[3]}
	} else {
		repo, number, ok := strings.Cut(input, "#")
		if !ok {
			return t, errors.New("expected OWNER/REPO#NUMBER or a pull request URL")
		}
		t = target{host, repo, number}
	}
	n, err := strconv.Atoi(t.Number)
	if !hostPattern.MatchString(t.Host) || !repoPattern.MatchString(t.Repo) || err != nil || n <= 0 {
		return target{}, errors.New("invalid host, repository, or pull request number")
	}
	t.Number = strconv.Itoa(n)
	return t, nil
}

func (p pullRequest) validate() error {
	if p.State != "open" || p.Merged {
		return errors.New("pull request is not open")
	}
	if p.Head.Repo == nil || p.Base.Repo == nil {
		return errors.New("head or base repository was deleted")
	}
	if !repoPattern.MatchString(p.Head.Repo.FullName) || !repoPattern.MatchString(p.Base.Repo.FullName) || !shaPattern.MatchString(p.Head.SHA) || !shaPattern.MatchString(p.Base.SHA) {
		return errors.New("invalid repository metadata from GitHub")
	}
	if p.Head.Repo.FullName != p.Base.Repo.FullName && !p.MaintainerCanModify {
		return errors.New("pull request does not allow edits by maintainers")
	}
	if p.Head.Repo.FullName == p.Base.Repo.FullName && p.Head.Ref == p.Base.Ref {
		return errors.New("head and base are the same branch")
	}
	return nil
}

type runner struct {
	ctx context.Context
	dir string
	env []string
}

func (r runner) command(name string, args ...string) (string, error) {
	cmd := exec.CommandContext(r.ctx, name, args...)
	cmd.Dir, cmd.Env = r.dir, r.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}
func cleanEnv() []string {
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GIT_") {
			env = append(env, e)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true", "GIT_SEQUENCE_EDITOR=true", "GH_PROMPT_DISABLED=1")
}
func getPR(r runner, t target) (pullRequest, error) {
	var p pullRequest
	out, err := r.command("gh", "api", "--hostname", t.Host, "repos/"+t.Repo+"/pulls/"+t.Number)
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		return p, err
	}
	return p, p.validate()
}
func samePR(a, b pullRequest) bool {
	return a.Head.Ref == b.Head.Ref && a.Head.SHA == b.Head.SHA && a.Head.Repo.FullName == b.Head.Repo.FullName && a.Base.Ref == b.Base.Ref && a.Base.SHA == b.Base.SHA && a.Base.Repo.FullName == b.Base.Repo.FullName
}
func rebase(r runner, base, head string, out io.Writer) (string, error) {
	if _, err := r.command("git", "checkout", "--detach", head); err != nil {
		return "", err
	}
	rebaseArgs := []string{"-c", "commit.gpgSign=false", "rebase", "--rebase-merges", "--no-fork-point", base}
	if _, err := r.command("git", rebaseArgs...); err != nil {
		if retryErr := retryWithMergiraf(r, rebaseArgs, out); retryErr != nil {
			return "", fmt.Errorf("rebase failed; nothing was pushed: %w\nMergiraf fallback: %w", err, retryErr)
		}
	}
	return r.command("git", "rev-parse", "HEAD")
}

// Retry from the original head using a merge driver, rather than guessing which
// conflicted files can safely be staged. Git still decides whether every commit
// (including recreated merges) has been resolved before the rebase can succeed.
func retryWithMergiraf(r runner, rebaseArgs []string, out io.Writer) error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	unmerged, err := r.command("git", "ls-files", "--unmerged")
	if err != nil {
		return err
	}
	if unmerged == "" {
		return errors.New("not attempted: no conflicted index entries")
	}
	tool, err := exec.LookPath("mergiraf")
	if err != nil {
		return fmt.Errorf("install mergiraf on PATH to try automatic conflict resolution: %w", err)
	}
	tool, err = filepath.Abs(tool)
	if err != nil {
		return err
	}
	attributes, err := r.command(tool, "languages", "--gitattributes")
	if err != nil {
		return err
	}
	if strings.TrimSpace(attributes) == "" {
		return errors.New("mergiraf returned no supported file patterns")
	}
	if _, err := r.command("git", "rebase", "--abort"); err != nil {
		return err
	}
	// Keep both config and attributes private to the disposable repository. %P
	// and the temporary paths are shell-quoted by Git when expanding the driver.
	driver := "'" + strings.ReplaceAll(tool, "'", "'\\''") + "' merge --git %O %A %B -p %P -l %L"
	for _, kv := range [][2]string{{"merge.mergiraf.driver", driver}, {"merge.conflictStyle", "diff3"}} {
		if _, err := r.command("git", "config", kv[0], kv[1]); err != nil {
			return err
		}
	}
	info := filepath.Join(r.dir, ".git", "info")
	if err := os.MkdirAll(info, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(info, "attributes"), []byte(attributes+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Fprintln(out, "Conflicts detected; retrying the rebase with Mergiraf…")
	result, err := r.command("git", rebaseArgs...)
	if err != nil {
		return fmt.Errorf("rebase with Mergiraf did not succeed: %w", err)
	}
	fmt.Fprintln(out, result)
	return nil
}
func push(r runner, remote, ref, old, newSHA string) error {
	_, err := r.command("git", "push", "--force-with-lease=refs/heads/"+ref+":"+old, remote, newSHA+":refs/heads/"+ref)
	return err
}

func run(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("gh-pr-rebase", flag.ContinueOnError)
	fs.SetOutput(out)
	defaultHost := os.Getenv("GH_HOST")
	if defaultHost == "" {
		defaultHost = "github.com"
	}
	host := fs.String("host", defaultHost, "GitHub host for OWNER/REPO#NUMBER (defaults to GH_HOST or github.com; URL host takes precedence)")
	dry := fs.Bool("dry-run", false, "perform the rebase locally without pushing")
	name := fs.String("committer-name", "", "committer name (default: authenticated GitHub user's name)")
	email := fs.String("committer-email", "", "committer email (default: authenticated GitHub user's noreply address)")
	fs.Usage = func() {
		fmt.Fprintln(out, "Usage: gh-pr-rebase [flags] https://github.com/OWNER/REPO/pull/NUMBER\n       gh-pr-rebase [flags] 'OWNER/REPO#NUMBER'\n\nRebases onto the current base branch and pushes with an explicit force-with-lease.\nRequires git and authenticated gh. Mergiraf is optional for conflicts. Flags must precede the PR.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("provide exactly one pull request")
	}
	t, err := parseTarget(fs.Arg(0), *host)
	if err != nil {
		return err
	}
	for _, tool := range []string{"git", "gh"} {
		if _, err := exec.LookPath(tool); err != nil {
			return err
		}
	}
	r := runner{ctx: ctx, env: cleanEnv()}
	p, err := getPR(r, t)
	if err != nil {
		return err
	}
	var user struct {
		Login, Name string
		ID          int64
	}
	data, err := r.command("gh", "api", "--hostname", t.Host, "user")
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(data), &user); err != nil {
		return err
	}
	if user.Login == "" {
		return errors.New("GitHub did not return an authenticated user")
	}
	if *name == "" {
		*name = user.Name
		if *name == "" {
			*name = user.Login
		}
	}
	if *email == "" {
		*email = fmt.Sprintf("%d+%s@users.noreply.github.com", user.ID, user.Login)
	}
	dir, err := os.MkdirTemp("", "gh-pr-rebase-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	r.dir = dir
	if _, err := r.command("git", "init", "--template=", "."); err != nil {
		return err
	}
	ghPath, err := exec.LookPath("gh")
	if err != nil {
		return err
	}
	helper := "!" + "'" + strings.ReplaceAll(ghPath, "'", "'\\''") + "' auth git-credential"
	for _, kv := range [][2]string{{"user.name", *name}, {"user.email", *email}, {"core.hooksPath", os.DevNull}, {"credential.helper", helper}, {"protocol.file.allow", "never"}, {"http.followRedirects", "false"}} {
		if _, err := r.command("git", "config", kv[0], kv[1]); err != nil {
			return err
		}
	}
	for _, b := range []branch{p.Base, p.Head} {
		if _, err := r.command("git", "check-ref-format", "refs/heads/"+b.Ref); err != nil {
			return err
		}
	}
	for _, item := range []struct {
		name string
		b    branch
	}{{"base", p.Base}, {"head", p.Head}} {
		remote := "https://" + t.Host + "/" + item.b.Repo.FullName + ".git"
		if _, err := r.command("git", "remote", "add", item.name, remote); err != nil {
			return err
		}
		if _, err := r.command("git", "fetch", "--no-tags", item.name, "refs/heads/"+item.b.Ref); err != nil {
			return err
		}
		sha, err := r.command("git", "rev-parse", "FETCH_HEAD")
		if err != nil {
			return err
		}
		if sha != item.b.SHA {
			return errors.New("branch changed while fetching; retry")
		}
	}
	fmt.Fprintf(out, "Rebasing %s:%s onto %s:%s…\n", p.Head.Repo.FullName, p.Head.Ref, p.Base.Repo.FullName, p.Base.Ref)
	newSHA, err := rebase(r, p.Base.SHA, p.Head.SHA, out)
	if err != nil {
		return err
	}
	if newSHA == p.Head.SHA {
		fmt.Fprintln(out, "Already up to date; nothing to push.")
		return nil
	}
	if *dry {
		fmt.Fprintf(out, "Dry run succeeded: %s → %s; nothing pushed.\n", p.Head.SHA, newSHA)
		return nil
	}
	latest, err := getPR(r, t)
	if err != nil {
		return err
	}
	if !samePR(p, latest) {
		return errors.New("pull request changed during rebase; nothing pushed; retry")
	}
	if err := push(r, "head", p.Head.Ref, p.Head.SHA, newSHA); err != nil {
		return err
	}
	fmt.Fprintf(out, "Pushed %s → %s.\n", p.Head.SHA, newSHA)
	return nil
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "gh-pr-rebase:", err)
		os.Exit(1)
	}
}
