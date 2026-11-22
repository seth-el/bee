#!/bin/sh
# Nightly versioning for Bee (run by bee-nightly-commit.timer at 03:00):
#   1. commit the workspace repo (memory, SOUL.md, BEE.md; see its
#      .gitignore) if anything changed, and push it;
#   2. export the redacted session history, commit it, and push it.
# A repo is pushed only when it has an "origin" remote. Every step runs even
# if an earlier one failed; the script exits nonzero if any failed, so the
# unit shows as failed. `bee doctor` (history_remotes) reports unpushed
# commits.
set -u
workspace="${BEE_WORKSPACE:-$HOME/.bee/workspace}"
history="${BEE_HISTORY:-$HOME/.bee/history}"
bee="${BEE_BIN:-$HOME/.local/bin/bee}"
status=0

push() {
	if git -C "$1" remote get-url origin >/dev/null 2>&1; then
		if ! git -C "$1" push -q origin HEAD; then
			echo "push failed: $1" >&2
			return 1
		fi
		echo "pushed $1"
	fi
}

if [ -d "$workspace/.git" ]; then
	if ! git -C "$workspace" add -A; then
		echo "workspace: git add failed" >&2
		status=1
	elif ! git -C "$workspace" diff --cached --quiet; then
		if git -C "$workspace" commit -q -m "Workspace snapshot $(date +%F)"; then
			echo "workspace: committed $(git -C "$workspace" rev-parse --short HEAD)"
		else
			echo "workspace: commit failed" >&2
			status=1
		fi
	else
		echo "workspace: nothing to commit"
	fi
	push "$workspace" || status=1
fi

"$bee" sessions export-history --commit || status=1
if [ -d "$history/.git" ]; then
	push "$history" || status=1
fi
exit "$status"
