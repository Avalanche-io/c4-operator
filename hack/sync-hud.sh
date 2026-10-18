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

# What the manager embeds (internal/hud/embed.go): the app, its fonts, and
# the design system without its fixtures. The intro (web/intro) is not
# part of the HUD.
mkdir -p "$dst/design"
cp "$src/web/index.html" "$dst/index.html"
for d in css js fonts; do rsync -a --delete "$src/web/$d/" "$dst/$d/"; done
cp "$src/web/design/tokens.css" "$src/web/design/README.md" "$dst/design/"
for d in components previews; do rsync -a --delete "$src/web/design/$d/" "$dst/design/$d/"; done

printf 'c4-hud %s\n' "$sha" > "$dst/VERSION"
echo "sync-hud: internal/hud/web is c4-hud $sha"

cd "$root"
go build ./...
go test -count=1 ./internal/hud/...
