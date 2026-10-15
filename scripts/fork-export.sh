#!/usr/bin/env bash
# Copyright 2026 G.G (https://github.com/gitaginanjar)
# SPDX-License-Identifier: Apache-2.0
#
# Exports the CYSTEMD fork of Nomad as a Git branch on top of upstream: a
# clone of hashicorp/nomad at the pinned release tag (checked against the
# pinned commit), then one commit for the overlay and one per patch. Push
# the branch to a GitHub fork of hashicorp/nomad to browse, review or
# rebase the fork with the usual Git tooling:
#
#   scripts/fork-export.sh                 # → build/nomad-fork, branch cystemd/v2.0.7
#   git -C build/nomad-fork push <your-fork> cystemd/v2.0.7
#
# The clone is shallow (the release commit only), which is all a push to a
# fork of hashicorp/nomad needs; FULL_HISTORY=1 clones the whole history.
# Builds do not use this: scripts/fork-assemble.sh assembles the same tree
# from the Go module proxy, verified by checksum.
set -euo pipefail

cd "$(dirname "$0")/.."
root=$PWD
# shellcheck source=/dev/null
source fork/nomad/UPSTREAM

dest=${1:-build/nomad-fork}
branch="cystemd/v${NOMAD_VERSION}"
tag="v${NOMAD_VERSION}"

if [ -e "$dest" ]; then
	echo "fork-export: $dest exists; remove it first" >&2
	exit 1
fi
if [ "${FULL_HISTORY:-0}" = 1 ]; then
	git clone --quiet --branch "$tag" https://github.com/hashicorp/nomad.git "$dest"
else
	git clone --quiet --depth 1 --branch "$tag" --single-branch https://github.com/hashicorp/nomad.git "$dest"
fi
commit=$(git -C "$dest" rev-parse "HEAD^{commit}")
if [ "$commit" != "$NOMAD_COMMIT" ]; then
	echo "fork-export: $tag is $commit, expected $NOMAD_COMMIT (fork/nomad/UPSTREAM)" >&2
	exit 1
fi
git -C "$dest" checkout --quiet -b "$branch"

# Commits use the local Git identity, or a neutral one when none is set.
git_commit() {
	if git -C "$dest" config user.email >/dev/null; then
		git -C "$dest" commit --quiet "$@"
	else
		git -C "$dest" -c user.name="CYSTEMD fork" -c user.email="cystemd-fork@users.noreply.github.com" commit --quiet "$@"
	fi
}

(cd fork/nomad/overlay && find . -type f | sort) | while read -r f; do
	f=${f#./}
	if [ -e "$dest/$f" ]; then
		echo "fork-export: overlay file $f exists upstream" >&2
		exit 1
	fi
	mkdir -p "$dest/$(dirname "$f")"
	cp "fork/nomad/overlay/$f" "$dest/$f"
done
git -C "$dest" add -A
git_commit -m "CYSTEMD fork: extension hook points (overlay)" \
	-m "New files only: the cystemd agent configuration block, the HTTP extension and web UI hooks, and the product version line."

for p in fork/nomad/patches/*.patch; do
	git -C "$dest" apply --whitespace=nowarn "$root/$p"
	subject=$(head -n1 "$p")
	body=$(awk 'NR > 2 && /^---$/ { exit } NR > 2 { print }' "$p")
	git -C "$dest" add -A
	git_commit -m "$subject" -m "$body"
done

echo "fork-export: branch $branch in $dest:"
git -C "$dest" log --oneline -n "$(($(ls fork/nomad/patches/*.patch | wc -l) + 2))"
