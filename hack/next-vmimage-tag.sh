#!/usr/bin/env bash
# Computes (and, by default, pushes) the next vmimage-v* release-candidate tag. Called from
# .github/workflows/vmimage-rc.yml on every PR push / main push touching guest-image paths. See
# RELEASING.md ("Releasing a new VM image") for the full RC -> graduate flow this feeds.
#
# Deliberately simple: rc -> rc+1, or stable/no-tag -> next-patch-rc.1. No conventional-commit
# analysis, no feat/fix/breaking-change inference -- that's what release-please already does for
# the CLI package; this only needs a provisional label for a throwaway candidate artifact. A
# bigger-than-patch bump is always available as a free choice at graduation time
# (vmimage-graduate.yml's `version` input), so the RC series' own number is not a commitment.
#
# Usage:
#   hack/next-vmimage-tag.sh [--dry-run] [--last-tag TAG]
#
#   --dry-run       print the computed tag but don't create/push it
#   --last-tag TAG  treat TAG as the latest vmimage-v* tag instead of querying `git tag -l` --
#                   lets the rc->rc+1 / stable->patch-rc.1 / no-prior-tag cases be exercised
#                   against a fabricated value, without needing real tag history in the repo
set -euo pipefail

dry_run=false
last_tag=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run)
      dry_run=true
      shift
      ;;
    --last-tag)
      last_tag="${2:?--last-tag requires a value}"
      shift 2
      ;;
    *)
      echo "error: unknown argument: $1" >&2
      exit 1
      ;;
  esac
done

if [[ -z "$last_tag" ]]; then
  top="$(git tag -l 'vmimage-v*' --sort=-v:refname | head -1)"
  # git's version:refname sort ranks tags by their X.Y.Z core correctly (a v0.5.1-rc.1 outranks
  # every v0.5.0.* tag, as it should), but it is NOT a real semver-precedence implementation: for
  # two tags sharing the same X.Y.Z core, it treats the bare vX.Y.Z as LOWER than its own
  # vX.Y.Z-rc.N siblings -- confirmed empirically, the opposite of semver's rule that a release
  # always outranks its own prereleases. Left uncorrected, this would mean the first push after a
  # graduation picks the stale rc tag back up instead of the just-graduated release, and keeps
  # bumping that old rc series (vX.Y.Z-rc.N+1) forever instead of starting vX.Y.(Z+1)-rc.1.
  #
  # Fix: take the winning X.Y.Z core from git's own top pick (that part is reliable), then
  # explicitly check whether the BARE tag for that core exists -- if so, it is the true latest,
  # regardless of where git's own sort put it relative to its rc siblings.
  if [[ "$top" =~ ^vmimage-v([0-9]+\.[0-9]+\.[0-9]+) ]] \
    && git rev-parse -q --verify "refs/tags/vmimage-v${BASH_REMATCH[1]}" >/dev/null; then
    last_tag="vmimage-v${BASH_REMATCH[1]}"
  else
    last_tag="$top"
  fi
fi

if [[ -z "$last_tag" ]]; then
  # No prior vmimage-v* tag at all: treat it as if vmimage-v0.0.0 already existed, so the "next
  # patch" formula below applies uniformly instead of needing a separate case.
  major=0 minor=0 patch=0
elif [[ "$last_tag" =~ ^vmimage-v([0-9]+)\.([0-9]+)\.([0-9]+)-rc\.([0-9]+)$ ]]; then
  major="${BASH_REMATCH[1]}" minor="${BASH_REMATCH[2]}" patch="${BASH_REMATCH[3]}"
  rc="${BASH_REMATCH[4]}"
  next_tag="vmimage-v${major}.${minor}.${patch}-rc.$((rc + 1))"
elif [[ "$last_tag" =~ ^vmimage-v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
  major="${BASH_REMATCH[1]}" minor="${BASH_REMATCH[2]}" patch="${BASH_REMATCH[3]}"
else
  echo "error: '$last_tag' doesn't look like a vmimage-v* tag (expected vmimage-vX.Y.Z or vmimage-vX.Y.Z-rc.N)" >&2
  exit 1
fi

# Reached by both "no prior tag" and "prior tag was a clean, graduated release" -- both start a
# new rc series at the next patch.
next_tag="${next_tag:-vmimage-v${major}.${minor}.$((patch + 1))-rc.1}"

echo "$next_tag"

if [[ "$dry_run" == true ]]; then
  exit 0
fi

git tag "$next_tag"
git push origin "$next_tag"
