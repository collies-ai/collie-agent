#!/bin/sh
# Fails when a commit in base..head (merges skipped) does not start with a Conventional Commit subject:
#   <type>[(scope)][!]: <description>
# with type one of build, chore, ci, docs, feat, fix, perf, refactor, revert, style, test.
# Usage: scripts/check-commits.sh <base> <head>
set -eu

base="$1"
head="$2"
pattern='^(build|chore|ci|docs|feat|fix|perf|refactor|revert|style|test)(\([^)]+\))?!?: .+'

bad="$(git log --no-merges --format='%h %s' "$base..$head" | while read -r sha subject; do
  printf '%s\n' "$subject" | grep -Eq "$pattern" || printf '%s %s\n' "$sha" "$subject"
done)"

if [ -n "$bad" ]; then
  echo "These commits are not Conventional Commits (<type>[(scope)][!]: <description>):" >&2
  printf '%s\n' "$bad" >&2
  echo "Types: build, chore, ci, docs, feat, fix, perf, refactor, revert, style, test. The type decides the release:" >&2
  echo "feat = minor, fix/perf/revert = patch, '!' or BREAKING CHANGE = major, the rest = no release." >&2
  exit 1
fi
