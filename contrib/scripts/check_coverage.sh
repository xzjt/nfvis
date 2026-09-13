#!/usr/bin/env bash
# 覆盖率门槛检查（Makefile `cover` 调用）。
# 用法: check_coverage.sh <pkg>...；COVER_MIN 门槛（默认 70），COVER_EXCLUDE 不计入的文件片段。
set -u
COVER_MIN="${COVER_MIN:-70}"
COVER_EXCLUDE="${COVER_EXCLUDE:-}"
GO="${GO:-go}"
DIR="$(cd "$(dirname "$0")" && pwd)"
fail=0
for pkg in "$@"; do
    prof="$(echo "$pkg" | tr '/' '_').cover"
    if ! $GO test -coverprofile="$prof" "$pkg" >/dev/null; then
        rm -f "$prof"
        exit 1
    fi
    pct="$(bash "$DIR/cover_pct.sh" "$prof" "$COVER_EXCLUDE")"
    if [ -n "$COVER_EXCLUDE" ]; then
        echo "$pkg 覆盖率: ${pct}%（不含 ${COVER_EXCLUDE}）"
    else
        echo "$pkg 覆盖率: ${pct}%"
    fi
    ok="$(awk -v p="$pct" -v m="$COVER_MIN" 'BEGIN { print (p >= m) ? 1 : 0 }')"
    if [ "$ok" != "1" ]; then
        echo "✗ $pkg 覆盖率 ${pct}% 低于门槛 ${COVER_MIN}%"
        fail=1
    fi
    rm -f "$prof"
done
exit $fail
