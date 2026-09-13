#!/usr/bin/env bash
# 计算 Go coverprofile 的语句覆盖率（百分比，保留一位小数）。
# 用法: cover_pct.sh <profile> [skip-substring]
# skip-substring 非空时，路径包含它的文件不计入（用于排除 *_govpp.go 适配层）。
set -eu
profile="$1"
skip="${2:-}"
awk -v skip="$skip" '
NR == 1 { next }
{
    split($1, a, ":")
    if (skip != "" && index(a[1], skip) > 0) next
    tot += $2
    if ($3 > 0) cov += $2
}
END { if (tot > 0) printf "%.1f", cov * 100 / tot; else print "0" }
' "$profile"
