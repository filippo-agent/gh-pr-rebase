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
# In a GitHub checkout, use gh's repository detection.
gh-pr-rebase --dry-run 123
gh-pr-rebase 123

# With no argument, use the current branch's PR (like gh pr view).
gh-pr-rebase

# Explicit targets also work outside a checkout.
gh-pr-rebase --dry-run https://github.com/OWNER/REPO/pull/123
gh-pr-rebase 'OWNER/REPO#123'
```

Put flags before the PR argument. A number (also `'#123'`) or no argument is
resolved by `gh pr view` in your current directory, using gh's remote/default
repository selection, `GH_REPO`, and current-branch detection. If gh cannot
identify a GitHub repository or PR, use an explicit URL or `OWNER/REPO#NUMBER`.
Discovery reads your normal Git environment; the rebase itself remains isolated.

For `OWNER/REPO#NUMBER`, `--host` defaults to `GH_HOST` or `github.com`. For an
inferred PR, the host comes from gh's returned URL unless `--host` is explicitly
set. An explicit PR URL always selects its own host. GitHub Enterprise hosts are
supported.

By default the authenticated user's profile name and GitHub noreply address are
used as the committer identity. Authors are preserved. Override with
`--committer-name` and `--committer-email` (recommended for Enterprise instances
with different email conventions).

## Automatic conflict resolution with Mergiraf

Optionally install [Mergiraf](https://mergiraf.org/installation.html) and put
`mergiraf` on `PATH` (tested with version 0.19.1). No Git configuration is needed.

The command first tries a normal Git rebase. If it stops with conflicts and
Mergiraf is available, it aborts that attempt and retries the entire rebase once
with Mergiraf's merge driver enabled for its supported file patterns. The driver
and attributes are configured **only in the temporary repository**, not globally
or in the PR. This also works with `--dry-run`.

If Mergiraf is missing, fails, or leaves any conflicts unresolved, nothing is
pushed. Non-conflict failures are not retried. Git must complete the entire
rebase successfully before a push is considered, and the same PR rechecks and
force-with-lease still apply. Mergiraf's retry output is shown so you can review
its work; syntax-aware resolution is not a guarantee of semantic correctness.
Use `--dry-run` first when you want to inspect the result before pushing.

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
- Unresolved conflicts fail without pushing, even after the optional Mergiraf
  retry. The temporary repository is removed on exit; resolve complicated
  rebases manually. `--dry-run` actually performs the local rebase, but never
  pushes. Already-current branches are left untouched.
- Rebases onto the actual fetched base-branch tip, not the potentially stale
  `base.sha` in GitHub's PR metadata. The fetched head must still match the PR's
  head SHA; a mismatch aborts with both SHAs in the error.
- Rechecks PR state, edit permission, branch names, repository identities, and
  head SHA before pushing. Separately rechecks the actual remote base tip to
  detect movement during the rebase. Push uses **an explicit expected-SHA
  force-with-lease**, so a contributor's concurrent push is not overwritten.
- GitHub does not offer a transaction spanning the base branch, PR metadata,
  and head push. The head SHA is protected atomically; the base or PR state can
  still change after the final API check. Re-run if the base advances again.
- During the rebase, hooks, global/system Git config, inherited `GIT_*`
  variables, submodules, and commit signing are not used. No PR code is executed. Rewritten commits
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
Mergiraf fallback tests use a fake driver; additional integration coverage runs
when the real `mergiraf` executable is on `PATH`.
