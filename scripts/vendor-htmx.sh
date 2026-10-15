#!/usr/bin/env bash
# Copyright 2026 G.G (https://github.com/gitaginanjar)
# SPDX-License-Identifier: Apache-2.0
#
# Vendors htmx and its server-sent-events extension into ui/static/vendor.
#
# The web UI is server-rendered Go (templ) and needs no JavaScript toolchain:
# these two prebuilt files are the only third-party browser code, served
# as-is by the binary. They are fetched from the package registry's release
# tarballs and verified against the pinned sha512 integrity values below
# before anything is extracted. To upgrade, change the version and the
# integrity (the registry publishes it as dist.integrity) and re-run.
set -euo pipefail

cd "$(dirname "$0")/.."
dest=ui/static/vendor
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# fetch NAME VERSION INTEGRITY SRC:DST...
fetch() {
	local name=$1 version=$2 integrity=$3
	shift 3
	local tgz="$work/$name-$version.tgz"
	curl -fsSL -o "$tgz" "https://registry.npmjs.org/$name/-/$name-$version.tgz"
	local got
	got="sha512-$(openssl dgst -sha512 -binary "$tgz" | base64 | tr -d '\n')"
	if [ "$got" != "$integrity" ]; then
		echo "vendor-htmx: integrity mismatch for $name@$version: got $got" >&2
		exit 1
	fi
	mkdir -p "$work/$name"
	tar -xzf "$tgz" -C "$work/$name"
	local pair
	for pair in "$@"; do
		install -m 0644 "$work/$name/package/${pair%%:*}" "$dest/${pair##*:}"
		echo "vendor-htmx: $name@$version ${pair%%:*} -> $dest/${pair##*:}"
	done
}

mkdir -p "$dest"
rm -f "$dest"/htmx-[0-9]*.min.js "$dest"/htmx-ext-sse-*.min.js

fetch htmx.org 2.0.11 \
	'sha512-Thx/WtpeOQqSrqBCw/A1cwGJGg4UrVa3+sW0GmrM3p4gJgO89ecH4qtbnyzDDWFvBTqjnIMCgELTNt636dtamA==' \
	dist/htmx.min.js:htmx-2.0.11.min.js LICENSE:LICENSE.htmx
fetch htmx-ext-sse 2.2.4 \
	'sha512-LJmxVhykyflBWgh5PvbRidcyuqMHlgfajmmzumvKctv9puvsufeH6OaejSMZTNTnEI8O2wXXn4ZZtBdpEMqmEQ==' \
	dist/sse.min.js:htmx-ext-sse-2.2.4.min.js LICENSE:LICENSE.htmx-ext-sse

(cd "$dest" && sha256sum htmx-[0-9]*.min.js htmx-ext-sse-*.min.js > SHA256SUMS)
echo "vendor-htmx: done"
