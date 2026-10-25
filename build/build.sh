#!/bin/bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="$(awk -F'=' '/^version[[:space:]]*=/{gsub(/[[:space:]]/,"",$2);print $2}' "$ROOT/manifest")"
CORE="${CORE_BIN:-/vol1/1000/harness/mihomo/.cache/mihomo-x86}"
DIST="$ROOT/dist"
info(){ printf '\033[32m==>\033[0m %s\n' "$*"; }
die(){ printf '\033[31m[✗]\033[0m %s\n' "$*" >&2; exit 1; }
[ -x "$CORE" ] || die "缺少 mihomo 内核：$CORE"
GO_BIN="${GO_BIN:-/vol1/1000/harness/.toolchain/go/bin/go}"
[ -x "$GO_BIN" ] || die "未找到 Go"
mkdir -p "$DIST" "$ROOT/app/bin" "$ROOT/.tmp"
STAGE="$DIST/stage"; PAYLOAD="$DIST/payload"
rm -rf "$STAGE" "$PAYLOAD"; mkdir -p "$STAGE" "$PAYLOAD/bin"
info "编译 proxyhub"
(cd "$ROOT/src" && CGO_ENABLED=0 GOCACHE="${GOCACHE:-$ROOT/.tmp/gocache}" GOPATH="${GOPATH:-$ROOT/.tmp/gopath}" \
  TMPDIR="${TMPDIR:-$ROOT/.tmp}" GOFLAGS=-mod=mod GOPROXY=off "$GO_BIN" build -trimpath -ldflags "-s -w" -o "$PAYLOAD/bin/proxyhub" .)
chmod 755 "$PAYLOAD/bin/proxyhub"
cp "$CORE" "$PAYLOAD/bin/mihomo"; chmod 755 "$PAYLOAD/bin/mihomo"
cp -a "$ROOT/app/ui" "$PAYLOAD/ui"
cp "$ROOT/manifest" "$STAGE/manifest"
cp -a "$ROOT/config" "$ROOT/cmd" "$ROOT/wizard" "$STAGE/"
cp "$ROOT/ICON.PNG" "$ROOT/ICON_256.PNG" "$STAGE/"
find "$STAGE" -type d -exec chmod 755 {} +; find "$STAGE" -type f -exec chmod 644 {} +; chmod 755 "$STAGE"/cmd/*
find "$PAYLOAD" -type d -exec chmod 755 {} +; find "$PAYLOAD" -type f -exec chmod 644 {} +; chmod 755 "$PAYLOAD/bin/proxyhub" "$PAYLOAD/bin/mihomo"
tar czf "$STAGE/app.tgz" -C "$PAYLOAD" --owner=0 --group=0 --numeric-owner --sort=name bin ui
chmod 644 "$STAGE/app.tgz"
OUT="$DIST/proxyhub-${VERSION}-x86.fpk"; rm -f "$OUT"
tar czf "$OUT" -C "$STAGE" --owner=0 --group=0 --numeric-owner --sort=name manifest cmd config wizard ICON.PNG ICON_256.PNG app.tgz
chmod 644 "$OUT"
( cd "$DIST" && sha256sum "$(basename "$OUT")" > "$(basename "$OUT").sha256" )
info "产物：$OUT（$(du -h "$OUT" | cut -f1)）"
info "校验：$(cat "$OUT.sha256")"
rm -rf "$STAGE" "$PAYLOAD"
