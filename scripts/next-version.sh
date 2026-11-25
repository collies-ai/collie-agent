#!/bin/sh
# Prints the next release tag from the Conventional Commits since the last v* tag, or nothing when there is nothing
# to release. Usage: scripts/next-version.sh [rev] (default HEAD).
#
#   no v* tag yet                                   -> v0.1.0
#   a breaking change (type!: or BREAKING CHANGE:)  -> major (minor while the version is 0.x)
#   feat                                            -> minor
#   fix, perf, revert, or a subject that is not a Conventional Commit -> patch
#   only build, chore, ci, docs, refactor, style, test -> nothing
#   rev already has a v* tag                        -> nothing
# Merge commits are skipped: the commits they bring in are counted.
set -eu

rev="${1:-HEAD}"

if [ -n "$(git tag --points-at "$rev" --list 'v*')" ]; then
  exit 0
fi

last="$(git tag --merged "$rev" --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | head -n1)"
if [ -z "$last" ]; then
  echo v0.1.0
  exit 0
fi

bump="$(git log --no-merges --format='%s%n%b%n--END--' "$last..$rev" | awk '
  function rank(b) { return b == "major" ? 3 : b == "minor" ? 2 : b == "patch" ? 1 : 0 }
  function take(b) { if (rank(b) > rank(best)) best = b }
  BEGIN { best = ""; subject = 1 }
  $0 == "--END--" { subject = 1; next }
  subject {
    subject = 0
    if (match($0, /^[a-z]+(\([^)]*\))?!?: /)) {
      type = $0; sub(/[(!:].*/, "", type)
      if ($0 ~ /^[a-z]+(\([^)]*\))?!: /) take("major")
      else if (type == "feat") take("minor")
      else if (type ~ /^(build|chore|ci|docs|refactor|style|test)$/) take("")
      else take("patch")
    } else {
      take("patch")
    }
    next
  }
  /^BREAKING[ -]CHANGE: / { take("major") }
  END { print best }
')"

[ -n "$bump" ] || exit 0

ver="${last#v}"
major="${ver%%.*}"
rest="${ver#*.}"
minor="${rest%%.*}"
patch="${rest#*.}"

if [ "$bump" = major ] && [ "$major" = 0 ]; then
  bump=minor
fi
case "$bump" in
  major) echo "v$((major + 1)).0.0" ;;
  minor) echo "v${major}.$((minor + 1)).0" ;;
  patch) echo "v${major}.${minor}.$((patch + 1))" ;;
esac
