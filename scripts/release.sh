#!/bin/sh
# Tags and publishes collie-agent releases. Run by .github/workflows/release.yml, serialized by its concurrency
# group. Every run catches up on what earlier runs left undone, so a run that GitHub dropped from the queue, failed
# or was canceled is repaired by the next one:
#
# 1. On main (TAG_COMMITS=true): every first-parent commit since the last v* tag that has a releasable change gets its
#    own tag, oldest first, with the version scripts/next-version.sh picks. With no tag yet, only HEAD is tagged
#    (v0.1.0).
# 2. Every v* tag (of the 20 highest) whose GitHub release is missing, a draft or lacks an asset is published with
#    GoReleaser, oldest first, replacing what a failed run left. Only the highest version is marked as the latest.
#
# Prints the tags it published, one per line, to $PUBLISHED_FILE (when set).
# Needs git, gh (GH_TOKEN) and goreleaser (GITHUB_TOKEN). DRY_RUN=true prints the pushes and publications instead.
set -eu

here="$(cd "$(dirname "$0")" && pwd)"
dry="${DRY_RUN:-false}"
assets="collie-agent_darwin_amd64.tar.gz collie-agent_darwin_arm64.tar.gz collie-agent_linux_amd64.tar.gz collie-agent_linux_arm64.tar.gz checksums.txt install.sh"

run() {
  if [ "$dry" = true ]; then
    echo "DRY_RUN: $*"
  else
    "$@"
  fi
}

git fetch --force --tags origin

if [ "${TAG_COMMITS:-false}" = true ]; then
  head="$(git rev-parse HEAD)"
  last="$(git tag --merged "$head" --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | head -n1)"
  if [ -z "$last" ]; then
    commits="$head"
  else
    commits="$(git rev-list --first-parent --reverse "$last..$head")"
  fi
  for commit in $commits; do
    tag="$("$here/next-version.sh" "$commit")"
    [ -n "$tag" ] || continue
    echo "Tagging $commit as $tag"
    git tag -a "$tag" -m "collie-agent $tag" "$commit"
    run git push origin "refs/tags/$tag"
  done
fi

complete() { # tag: the release exists, is published and has every asset
  names="$(gh release view "$1" --json isDraft,assets --jq 'select(.isDraft | not) | .assets[].name' 2>/dev/null)" || return 1
  for asset in $assets; do
    printf '%s\n' "$names" | grep -qxF "$asset" || return 1
  done
}

latest="$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | head -n1)"
start="$(git rev-parse HEAD)"
for tag in $(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=v:refname | tail -n 20); do
  if complete "$tag"; then
    continue
  fi
  echo "Publishing $tag"
  if [ "$tag" = "$latest" ]; then make_latest=true; else make_latest=false; fi
  git checkout --quiet --detach "refs/tags/$tag"
  MAKE_LATEST="$make_latest" run goreleaser release --clean
  [ -z "${PUBLISHED_FILE:-}" ] || echo "$tag" >>"$PUBLISHED_FILE"
done
git checkout --quiet --detach "$start"
