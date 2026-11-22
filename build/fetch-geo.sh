#!/bin/bash
# 下载打包所需的地理数据到 app/share/geo/。
# 已存在的文件会跳过；构建脚本在文件缺失时自动调用。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEST="${1:-$ROOT/app/share/geo}"
mkdir -p "$DEST"

info() { printf '\033[32m==>\033[0m %s\n' "$*"; }
die() { printf '\033[31m[✗]\033[0m %s\n' "$*" >&2; exit 1; }

fetch() {
    local remote="$1" local_name="$2" min="$3" base url
    if [ -s "$DEST/$local_name" ]; then
        info "已存在 $local_name"
        return 0
    fi
    for base in \
        "https://cdn.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@release" \
        "https://gcore.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@release" \
        "https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest"; do
        url="$base/$remote"
        info "下载 $local_name <- $url"
        if curl -fsSL -m 300 -o "$DEST/$local_name.download" "$url" \
            && [ "$(wc -c < "$DEST/$local_name.download")" -ge "$min" ]; then
            mv "$DEST/$local_name.download" "$DEST/$local_name"
            chmod 644 "$DEST/$local_name"
            return 0
        fi
        rm -f "$DEST/$local_name.download"
    done
    die "无法下载 $local_name"
}

fetch country.mmdb Country.mmdb 4000000
fetch geosite.dat GeoSite.dat 2000000
fetch geoip.dat GeoIP.dat 8000000
info "地理数据已就绪：$DEST"
