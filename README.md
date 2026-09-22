# gh-pr-rebase

Rebase a GitHub pull request onto its current base branch, then push the result
back to the contributor's branch. Uses your existing **GitHub CLI (`gh`) auth**
for both the API and Git over HTTPS. No extra token, checkout, or Go dependencies.

## Install

Requires Go 1.22+ to install, and `git` and `gh` at runtime.

```sh
go install github.com/filippo-agent/gh-pr-rebase@latest
gh auth login
```

## Use

```sh
# Preview the rebase without pushing.
gh-pr-rebase --dry-run https://github.com/OWNER/REPO/pull/123

# Rebase and push.
gh-pr-rebase https://github.com/OWNER/REPO/pull/123
# Or:
gh-pr-rebase 'OWNER/REPO#123'
```

Run from any directory. Put flags before the PR argument. `--host` (default:
`GH_HOST`, or `github.com`) selects the host for shorthand arguments; a PR URL
always selects its own host. GitHub Enterprise hosts are supported.

By default the authenticated user's profile name and GitHub noreply address are
used as the committer identity. Authors are preserved. Override with
`--committer-name` and `--committer-email` (recommended for Enterprise instances
with different email conventions).

## Behavior and safety

- The PR must be open, with both repositories still available.
- For fork PRs, **Allow edits from maintainers** must be enabled. For same-repo
  PRs, normal write permissions apply. Your account/token must have permission
  to push; enabling maintainer edits does not give arbitrary users access.
- Fetches both branches into an isolated temporary repository. Never changes
  your working tree or Git configuration. Full history is fetched, not a shallow
  clone; large repositories may take time.
- Uses `git rebase --rebase-merges --no-fork-point` onto the PR's actual base,
  not a hard-coded `main` branch. Git's usual duplicate/empty-commit behavior
  applies; merge topology is recreated, but old merge resolutions may need to
  be redone and can cause the operation to fail.
- Conflicts fail without pushing. The temporary repository is removed on exit;
  resolve complicated rebases manually. `--dry-run` actually performs the local
  rebase, but never pushes. Already-current branches are left untouched.
- Rechecks PR state, edit permission, branch names, repositories, and commit
  IDs before pushing. Push uses **an explicit expected-SHA force-with-lease**,
  so a contributor's concurrent push is not overwritten.
- GitHub does not offer a transaction spanning the base branch, PR metadata,
  and head push. The head SHA is protected atomically; the base or PR state can
  still change after the final API check. Re-run if the base advances again.
- Hooks, global/system Git config, inherited `GIT_*` variables, submodules,
  and commit signing are not used. No PR code is executed. Rewritten commits
  lose their signatures; branch protections requiring signed commits or
  forbidding force pushes can reject the push. Custom Git proxy/CA/config
  settings are not inherited.
- `gh auth git-credential` supplies HTTPS credentials on demand. Tokens are not
  printed or written into the temporary Git config. Existing `gh` login and
  environment-based auth both work. GitHub still enforces token scopes,
  organization policies, and branch protections.

Pushing rewritten history is the default: use `--dry-run` first if unsure.

## Development

```sh
go test -race ./...
go vet ./...
go build .
```

Tests use disposable local Git repositories and do not require GitHub access.
