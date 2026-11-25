#!/bin/sh
# Tags and publishes collie-agent releases. Run by .github/workflows/release.yml, serialized by its concurrency
# group. Whatever triggered it (a push to main, a v* tag, a manual run), every run reconciles everything, so a run
# that GitHub dropped from the queue, failed or was canceled is repaired by the next one:
#
# 1. Tags main: every first-parent commit of origin/main since the last v* tag that has a releasable change gets its
#    own tag, oldest first, with the version scripts/next-version.sh picks. With no tag yet, it starts at the commit
#    that added this script (the first release is v0.1.0).
# 2. Publishes every v* tag whose GitHub release is missing, a draft or lacks an asset, oldest first, replacing what
#    a failed run left. Only the highest version is marked as the latest release.
#
# Prints the tags it published, one per line, to $PUBLISHED_FILE (when set).
# Needs git, gh (GH_TOKEN, GITHUB_REPOSITORY) and goreleaser (GITHUB_TOKEN). DRY_RUN=true prints the pushes and
# publications instead.
set -eu

here="$(cd "$(dirname "$0")" && pwd)"
dry="${DRY_RUN:-false}"
assets="collie-agent_darwin_amd64.tar.gz collie-agent_darwin_arm64.tar.gz collie-agent_linux_amd64.tar.gz collie-agent_linux_arm64.tar.gz checksums.txt install.sh"
versions='v[0-9]*.[0-9]*.[0-9]*'

run() {
  if [ "$dry" = true ]; then
    echo "DRY_RUN: $*"
  else
    "$@"
  fi
}

git fetch --force --tags origin "+refs/heads/main:refs/remotes/origin/main"
main="$(git rev-parse refs/remotes/origin/main)"

last="$(git tag --merged "$main" --list "$versions" --sort=-v:refname | head -n1)"
if [ -n "$last" ]; then
  commits="$(git rev-list --first-parent --reverse "$last..$main")"
else
  # No release yet: start at the first-parent commit of main that added this script.
  first="$(git rev-list --first-parent "$main" -- scripts/release.sh | tail -n1)"
  [ -n "$first" ] || first="$main"
  if git rev-parse --quiet --verify "$first^" >/dev/null; then
    commits="$(git rev-list --first-parent --reverse "$first^..$main")"
  else
    commits="$(git rev-list --first-parent --reverse "$main")"
  fi
fi
for commit in $commits; do
  tag="$("$here/next-version.sh" "$commit")"
  [ -n "$tag" ] || continue
  echo "Tagging $commit as $tag"
  git tag -a "$tag" -m "collie-agent $tag" "$commit"
  run git push origin "refs/tags/$tag"
done

# Every release with its assets, in one paginated call: tag<TAB>asset (or tag<TAB> for a release without assets).
# shellcheck disable=SC2016 # $t is a jq variable
published="$(gh api --paginate "repos/${GITHUB_REPOSITORY}/releases" \
  --jq '.[] | select(.draft | not) | .tag_name as $t | if (.assets | length) == 0 then "\($t)\t" else .assets[] | "\($t)\t\(.name)" end')"

release_complete() { # tag
  for asset in $assets; do
    printf '%s\n' "$published" | grep -qxF "$(printf '%s\t%s' "$1" "$asset")" || return 1
  done
}

latest="$(git tag --list "$versions" --sort=-v:refname | head -n1)"
start="$(git rev-parse HEAD)"
for tag in $(git tag --list "$versions" --sort=v:refname); do
  if release_complete "$tag"; then
    continue
  fi
  echo "Publishing $tag"
  if [ "$tag" = "$latest" ]; then make_latest=true; else make_latest=false; fi
  git checkout --quiet --detach "refs/tags/$tag"
  # GORELEASER_CURRENT_TAG: build this tag even when another version tag points at the same commit.
  GORELEASER_CURRENT_TAG="$tag" MAKE_LATEST="$make_latest" run goreleaser release --clean
  [ -z "${PUBLISHED_FILE:-}" ] || echo "$tag" >>"$PUBLISHED_FILE"
done
git checkout --quiet --detach "$start"
