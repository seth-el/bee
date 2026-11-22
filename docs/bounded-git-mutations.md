# Bounded local Git stage/commit

> **Not model-facing since 2026-11-12.** The main assistant no longer offers the tools below (`git_*`) (`prunedForegroundTools` in `assistant.go`; usage review in AGENTS.md). They remain implemented and tested, and restoring one is a one-line change.

The ordinary trusted or prompted parent assistant exposes `git_staged_review`,
`git_stage`, and `git_commit`. These tools are deliberately narrower than
shell access and are absent from locked/provider-child compositions and all delegated child projections.

`git_stage` accepts only an explicit list of exact repository-relative regular
files, safe leaf symlinks, or tracked deletions. Symlinked parents, dangling or
external symlink targets, `.git` files/symlinks, worktrees, external
`core.worktree`/common directories, and special files are rejected. It also
rejects globs, directories, pathspec syntax, submodules/gitlinks,
unsupported index flags/extensions, sparse/split indexes, and
merge/rebase/cherry-pick/revert/bisect states. It hashes file content directly
and uses Git's real `.git/index.lock` protocol: the complete validated index is
written and fsynced to the lock, then atomically renamed over the index and the
metadata directory is fsynced. Git clean/smudge/process filters are not run.

`git_staged_review` returns bounded complete staged diff evidence and a
cryptographic `review_hash` bound to the repository, branch/base HEAD, index,
exact staged paths, and complete staged diff. `git_commit` requires that exact
hash, recomputes it immediately before creating a one-parent local commit, and
uses guarded `update-ref` against the reviewed HEAD. It uses plumbing only:
there is no push/fetch/pull, merge/rebase/cherry-pick/revert/reset/checkout,
hook, signing, editor, prompt, credential-helper, remote, or generic config
authority. Commits require bounded repository-local `user.name` and
`user.email`; global/system identity is never used. Output is bounded local
evidence only.

This is source-only implementation work. Deployment is intentionally separate.
