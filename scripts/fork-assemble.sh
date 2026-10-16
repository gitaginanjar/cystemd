#!/usr/bin/env bash
# Assemble the CYSTEMD Nomad fork into a build tree:
#
#   pinned upstream Nomad source (fork/nomad/UPSTREAM, checksum-verified)
#   + overlay files       (fork/nomad/overlay — new files only, never edits)
#   + patch series        (fork/nomad/patches/*.patch, applied in order)
#
# Usage: scripts/fork-assemble.sh [OUT_DIR]   (default: build/nomad)
#
# The tree is a build artefact (gitignored). The cystemd binary (agent/)
# compiles against it through a replace directive. See docs/FORK.md.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FORK="$ROOT/fork/nomad"
OUT="${1:-$ROOT/build/nomad}"
GO="${GO:-go}"

# shellcheck source=../fork/nomad/UPSTREAM
source "$FORK/UPSTREAM"

log() { printf 'fork-assemble: %s\n' "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

# download MODULE VERSION WANT_SUM → prints the extracted directory.
# `go mod download` verifies the zip against the checksum database; the
# comparison with the pinned sum additionally guards against a changed pin.
download() {
  local module=$1 version=$2 want=$3 json dir sum
  json="$(cd "$(mktemp -d)" && GOFLAGS=-mod=mod "$GO" mod download -json "$module@$version")" \
    || die "cannot download $module@$version"
  dir="$(printf '%s' "$json" | sed -n 's/^[[:space:]]*"Dir": "\(.*\)",$/\1/p')"
  sum="$(printf '%s' "$json" | sed -n 's/^[[:space:]]*"Sum": "\(.*\)",$/\1/p')"
  [[ -n "$dir" && -d "$dir" ]] || die "no source directory for $module@$version"
  [[ "$sum" == "$want" ]] || die "$module@$version: checksum $sum != pinned $want"
  printf '%s' "$dir"
}

log "upstream: Nomad $NOMAD_VERSION ($NOMAD_COMMIT)"
nomad_dir="$(download "$NOMAD_MODULE" "$NOMAD_MODULE_VERSION" "$NOMAD_MODULE_SUM")"
api_dir="$(download "$NOMAD_MODULE/api" "$NOMAD_API_MODULE_VERSION" "$NOMAD_API_MODULE_SUM")"
jobspec2_dir="$(download "$NOMAD_MODULE/jobspec2" "$NOMAD_JOBSPEC2_MODULE_VERSION" "$NOMAD_JOBSPEC2_MODULE_SUM")"

log "assembling into $OUT"
rm -rf "$OUT"
mkdir -p "$(dirname "$OUT")"
cp -R "$nomad_dir" "$OUT"
cp -R "$api_dir" "$OUT/api"
cp -R "$jobspec2_dir" "$OUT/jobspec2"
chmod -R u+w "$OUT"

# Overlay: new files only. An overlay path that already exists upstream is a
# silent edit of Nomad and must be a patch instead.
if [[ -d "$FORK/overlay" ]]; then
  while IFS= read -r -d '' f; do
    rel="${f#"$FORK/overlay/"}"
    [[ ! -e "$OUT/$rel" ]] || die "overlay file $rel exists upstream — express the change as a patch"
    mkdir -p "$(dirname "$OUT/$rel")"
    cp "$f" "$OUT/$rel"
    log "overlay: $rel"
  done < <(find "$FORK/overlay" -type f -print0 | sort -z)
fi

# Patches: applied in lexical order, with no fuzz, so a context change upstream
# fails loudly instead of landing somewhere else.
shopt -s nullglob
for p in "$FORK"/patches/*.patch; do
  log "patch: $(basename "$p")"
  patch -d "$OUT" -p1 --forward --batch --fuzz=0 --no-backup-if-mismatch --quiet < "$p" \
    || die "patch $(basename "$p") does not apply to Nomad $NOMAD_VERSION"
done

cat > "$OUT/CYSTEMD_FORK" <<EOF
This tree is the CYSTEMD fork of HashiCorp Nomad, assembled by
scripts/fork-assemble.sh from:
  upstream: $NOMAD_MODULE $NOMAD_VERSION ($NOMAD_COMMIT)
  overlay:  fork/nomad/overlay
  patches:  $(cd "$FORK/patches" && ls ./*.patch 2>/dev/null | tr '\n' ' ')
Licensed under the Business Source License 1.1 (see LICENSE).
EOF

log "done: $OUT"
