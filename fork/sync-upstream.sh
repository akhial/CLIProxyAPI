#!/usr/bin/env bash
# Replay the fork's commits onto an upstream release tag and verify the result.
#
# Usage: fork/sync-upstream.sh <repo-dir> [upstream-tag]
#   <repo-dir>      backend (CLIProxyAPI) or frontend (Cli-Proxy-API-Management-Center) checkout
#   [upstream-tag]  defaults to the newest stable upstream GitHub release
#
# Leaves the result on branch sync/<tag>; main and origin are not touched.
# Prints the push command once the checks pass. See fork/PLAYBOOK.md.
set -euo pipefail

REPO=$(realpath "${1:?usage: sync-upstream.sh <repo-dir> [upstream-tag]}")
TAG=${2:-}
SCRIPTS=$(cd "$(dirname "$0")" && pwd)
cd "$REPO"

say() { printf '\n== %s\n' "$*"; }
die() {
  printf 'ABORT: %s\n' "$*" >&2
  exit 1
}

git remote get-url upstream >/dev/null 2>&1 || die "no 'upstream' remote in $REPO"
[[ -z $(git status --porcelain) ]] || die "working tree is not clean"
[[ $(git branch --show-current) == main ]] || die "check out main first"

say "fetching"
git fetch --quiet origin
git fetch --quiet --tags upstream
[[ $(git rev-parse main) == $(git rev-parse origin/main) ]] ||
  die "local main differs from origin/main; push or reset it first"

# Default: the newest stable (non-prerelease) GitHub release of upstream, else
# the newest tag reachable from upstream/main.
latest_stable_release() {
  local slug
  slug=$(git remote get-url upstream | sed -nE 's#^(https://github\.com/|git@github\.com:)([^/]+/[^/.]+)(\.git)?$#\2#p')
  [[ -n $slug ]] && command -v gh >/dev/null || return 0
  gh release list --repo "$slug" --exclude-pre-releases --exclude-drafts --limit 1 \
    --json tagName --jq '.[0].tagName // empty' 2>/dev/null || true
}
TAG=${TAG:-$(latest_stable_release)}
TAG=${TAG:-$(git describe --tags --abbrev=0 upstream/main)}
git rev-parse -q --verify "refs/tags/$TAG" >/dev/null || die "unknown tag $TAG"
OLD_BASE=$(git merge-base main upstream/main)
if git merge-base --is-ancestor "$TAG" main; then
  say "main already contains $TAG; nothing to do"
  exit 0
fi

say "fork commits on top of $(git describe --tags --always "$OLD_BASE")"
git log --oneline "$OLD_BASE..main"
say "upstream changes up to $TAG ($(git rev-list --count "$OLD_BASE..$TAG") commits); upstream files the fork also edits:"
LC_ALL=C comm -12 \
  <(git diff --name-only "$OLD_BASE" "$TAG" | LC_ALL=C sort) \
  <(git diff --name-only "$OLD_BASE" main | LC_ALL=C sort) || true

BRANCH=sync/$TAG
git branch -f "backup/main-$(date -u +%Y%m%d)-pre-$TAG" main
git switch -q -C "$BRANCH" main
say "rebasing onto $TAG"
if ! git rebase --onto "$TAG" "$OLD_BASE" "$BRANCH"; then
  cat <<EOF

Rebase stopped on a conflict. Resolve it (see "Conflict hotspots" in
fork/PLAYBOOK.md), then: git add <files> && git rebase --continue
When the rebase finishes, run: $SCRIPTS/verify.sh $REPO $TAG
To give up: git rebase --abort && git switch main
EOF
  exit 2
fi

say "verifying"
"$SCRIPTS/verify.sh" . "$TAG"

OLD_MAIN=$(git rev-parse main)
cat <<EOF

Rebased cleanly onto $TAG and all checks passed (branch $BRANCH).
Review:   git log --oneline $TAG..$BRANCH
Publish:  git push --force-with-lease=main:$OLD_MAIN origin $BRANCH:main && git switch main && git reset --hard origin/main
Backup:   branch backup/main-$(date -u +%Y%m%d)-pre-$TAG keeps the previous main locally.
EOF
