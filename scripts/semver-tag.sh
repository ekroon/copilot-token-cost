#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "Usage: $0 {major|minor|patch}"
}

if ! command -v git >/dev/null 2>&1; then
  echo "Error: git is required." >&2
  exit 1
fi

if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  echo "Error: must be run inside a git repository." >&2
  exit 1
fi

if ! git remote get-url origin >/dev/null 2>&1; then
  echo "Error: git remote 'origin' is not configured." >&2
  exit 1
fi

bump="${1:-}"
if [[ $# -ne 1 || ! "$bump" =~ ^(major|minor|patch)$ ]]; then
  usage
  exit 1
fi

latest_tag="$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n 1 || true)"
if [[ -z "$latest_tag" ]]; then
  latest_tag="v0.0.0"
fi

if [[ ! "$latest_tag" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
  echo "Error: latest semver tag '$latest_tag' is invalid." >&2
  exit 1
fi

major="${BASH_REMATCH[1]}"
minor="${BASH_REMATCH[2]}"
patch="${BASH_REMATCH[3]}"

case "$bump" in
  major)
    major=$((major + 1))
    minor=0
    patch=0
    ;;
  minor)
    minor=$((minor + 1))
    patch=0
    ;;
  patch)
    patch=$((patch + 1))
    ;;
esac

new_tag="v${major}.${minor}.${patch}"
if git rev-parse "$new_tag" >/dev/null 2>&1; then
  echo "Error: tag '$new_tag' already exists." >&2
  exit 1
fi

git tag -a "$new_tag" -m "Release $new_tag"
git push origin "$new_tag"

echo "Latest semver tag: $latest_tag"
echo "Created and pushed: $new_tag"
