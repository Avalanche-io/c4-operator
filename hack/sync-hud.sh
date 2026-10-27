#!/usr/bin/env bash
# sync-hud.sh copies the HUD web app from a c4-hud checkout into
# internal/hud/web, the copy the manager embeds, and records the c4-hud
# commit it came from in internal/hud/web/VERSION. internal/hud/web is a
# synced copy: never edit it by hand; change c4-hud and sync again.
#
# Until the operator embeds github.com/Avalanche-io/c4-hud/web directly
# (once c4-hud is public with a tag), this is how the operator ships the
# current HUD.
#
# usage: hack/sync-hud.sh [path to c4-hud]   (default ../c4-hud)
set -euo pipefail

root=$(git rev-parse --show-toplevel)
src=${1:-$root/../c4-hud}
src=$(cd "$src" && pwd)
dst=$root/internal/hud/web

test -f "$src/web/index.html" || { echo "sync-hud: $src is not a c4-hud checkout" >&2; exit 1; }
sha=$(git -C "$src" rev-parse HEAD)
if [ -n "$(git -C "$src" status --porcelain -- web)" ]; then
  echo "sync-hud: $src/web has uncommitted changes; commit them first so VERSION names what was copied" >&2
  exit 1
fi

# A hand edit to internal/hud/web is lost at the next sync: refuse while a
# file differs both from what the c4-hud commit VERSION names copied and
# from what this sync copies (a ported edit matches the latter). Port the
# edit to c4-hud first; SYNC_FORCE=1 overwrites on purpose.
prev=$(awk '{print $2}' "$dst/VERSION" 2>/dev/null || true)
if [ -n "$prev" ] && [ -z "${SYNC_FORCE:-}" ] && git -C "$src" cat-file -e "$prev^{commit}" 2>/dev/null; then
  was=$(mktemp -d)
  trap 'rm -rf "${was:?}"' EXIT
  git -C "$src" archive "$prev" web | tar -x -C "$was"
  # ported: the hand edit's own hunks are in this sync's source already, so
  # taking them back out of it applies cleanly
  # (diff exits 1 on any difference: under pipefail only patch's status counts)
  ported() { { diff -u "$was/web/$1" "$1" || true; } | patch -R --dry-run -s -f -o /dev/null "$src/web/$1" >/dev/null 2>&1; }
  edited=$(cd "$dst" && find . -type f ! -name VERSION | sort | while read -r f; do cmp -s "$f" "$was/web/$f" || cmp -s "$f" "$src/web/$f" || ported "$f" || echo "${f#./}"; done)
  if [ -n "$edited" ]; then
    echo "sync-hud: internal/hud/web was edited by hand since c4-hud ${prev:0:7}; port these to c4-hud first (or SYNC_FORCE=1 to overwrite):" >&2
    echo "$edited" | sed 's/^/  /' >&2
    exit 1
  fi
fi

# What the manager embeds (internal/hud/embed.go): the app, its fonts, and
# the design system without its fixtures. The intro (web/intro) is not
# part of the HUD.
mkdir -p "$dst/design"
cp "$src/web/index.html" "$dst/index.html"
for d in css js fonts; do rsync -a --delete "$src/web/$d/" "$dst/$d/"; done
# the design system: tokens, components, the scheme modules and previews;
# not its fixtures or its build scripts (*.mjs)
rsync -a --delete --exclude fixtures/ --exclude '*.mjs' "$src/web/design/" "$dst/design/"

printf 'c4-hud %s\n' "$sha" > "$dst/VERSION"
echo "sync-hud: internal/hud/web is c4-hud $sha"

cd "$root"
go build ./...
go test -count=1 ./internal/hud/...
