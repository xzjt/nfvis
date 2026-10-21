#!/usr/bin/env bash
# 发布件**跨版本对账**：两份 `.run` 的载荷 deb 清单必须可解释地一致。
#
# 由来（round41 v3.0.24，同一次发布里错了三次中的两次）：
#   · 首轮载荷 236 deb（少 3 个 VPP 包），而结构自检 **18/0 全绿**；
#   · 载荷里还混进一份同包名副本（`deb-pass1.deb`），自检同样全绿；
#   两次都是用户追问「前面不都是 239 个 deb 吗」才发现的。
#   **教训**：结构自检证明「内部自洽」，跨版本对账才证明「内容没漂」——
#   包数少了、混进多余文件，只要索引与目录同步，自检照样通过。
#
# 用法：
#   check_release_delta.sh OLD.run NEW.run
#
# 判据（任一不满足即非零退出）：
#   1. 两个载荷都能解包，且都能列出 deb 清单；
#   2. **包名集合**的差异只能来自「预期变更」：`nfvis`（版本必变）、以及**上游包版本号演进**
#      （同一包名不同版本，如 systemd 259.5→259.9）——这类允许，但要逐条打印出来供人过目；
#   3. **新增/消失的包名**必须显式登记在 `ALLOW_ADD`/`ALLOW_DROP` 里（空格分隔的包名），
#      否则判失败——这正是「少带 3 个 VPP 包」能被抓住的地方；
#   4. 两边的**总包数**都要打印；差异过大（>DELTA_WARN）额外告警（防整批漏带）。
#
# 自校准（--selftest）：造两个玩具载荷，断言「只差版本号 → 通过」「少一个包 → 报失败」。
set -u

die() { printf '错误：%s\n' "$*" >&2; exit 2; }
have() { command -v "$1" >/dev/null 2>&1; }

# 允许的变更（按需在命令行覆盖）
ALLOW_ADD=${ALLOW_ADD:-}
ALLOW_DROP=${ALLOW_DROP:-}
DELTA_WARN=${DELTA_WARN:-15}

# 解包并列出「包名<TAB>文件名」清单。用 --extract（.run 自带的能力），失败即退出。
list_debs() { # $1=.run  $2=工作目录
    local run=$1 work=$2
    rm -rf "$work"; mkdir -p "$work"
    if ! sh "$run" --extract "$work" >/dev/null 2>&1; then
        die "解包失败：$run（需要 root？或文件损坏）"
    fi
    [ -d "$work/debs" ] || die "载荷里没有 debs/ 目录：$run"
    local d n
    for d in "$work"/debs/*.deb; do
        [ -e "$d" ] || continue
        n=$(dpkg-deb -f "$d" Package 2>/dev/null)
        [ -n "$n" ] || die "读不出包名：$d"
        printf '%s\t%s\n' "$n" "$(basename "$d")"
    done | sort
}

# 自校准：用最小的假载荷验证判据本身（不依赖真 .run）
if [ "${1:-}" = "--selftest" ]; then
    have dpkg-deb || { echo "  ! 本机没有 dpkg-deb，跳过自校准"; exit 0; }
    TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
    mkdir -p "$TMP/A/debs" "$TMP/B/debs" "$TMP/C/debs"
    # 造玩具 deb：**先建 DEBIAN 目录再写 control**（顺序反了 mkdir -p 会报「已存在」且文件写不进去，
    # 于是 dpkg-deb -b 失败、载荷是空的——首版自校准就因此成了「少包也判通过」的假绿）
    mkb() { # mkb <目标目录> <包名> <版本>
        local tmp="$TMP/mk.$$"
        rm -rf "$tmp"
        mkdir -p "$tmp/DEBIAN"
        printf 'Package: %s
Version: %s
Architecture: amd64
Maintainer: t <t@t>
Description: t
'             "$2" "$3" > "$tmp/DEBIAN/control"
        dpkg-deb --root-owner-group -b "$tmp" "$1/debs/$2_$3.deb" >/dev/null 2>&1             || die "自校准用例构造失败：$2 $3"
        rm -rf "$tmp"
    }
    mkb "$TMP/A" aaa 1.0; mkb "$TMP/A" bbb 1.0
    mkb "$TMP/B" aaa 2.0; mkb "$TMP/B" bbb 1.0      # 只差版本号 → 应通过
    mkb "$TMP/C" aaa 2.0                              # 少一个包 → 应失败
    r1=0; bash "$0" --dir "$TMP/A" --dir2 "$TMP/B" >/dev/null 2>&1 || r1=$?
    r2=0; bash "$0" --dir "$TMP/A" --dir2 "$TMP/C" >/dev/null 2>&1 || r2=$?
    if [ "$r1" -eq 0 ]; then echo "  ✓ 自校准：只有版本号变化 → 判通过"; else echo "  ✗ 自校准失败：仅版本变化被判失败"; exit 1; fi
    if [ "$r2" -ne 0 ]; then echo "  ✓ 自校准：少一个包 → 判失败"; else echo "  ✗ 自校准失败：少包被判通过（判据形同空转）"; exit 1; fi
    exit 0
fi

# --dir/--dir2：直接对已解包的载荷目录比对（自校准与调试用）
if [ "${1:-}" = "--dir" ]; then
    A=${2:?}; B=${4:?}
    # shellcheck disable=SC2016
    la=$(for d in "$A"/debs/*.deb; do dpkg-deb -f "$d" Package 2>/dev/null; done | sort)
    lb=$(for d in "$B"/debs/*.deb; do dpkg-deb -f "$d" Package 2>/dev/null; done | sort)
else
    OLD=${1:?用法：check_release_delta.sh OLD.run NEW.run}; NEW=${2:?用法：check_release_delta.sh OLD.run NEW.run}
    TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
    la=$(list_debs "$OLD" "$TMP/a" | cut -f1)
    lb=$(list_debs "$NEW" "$TMP/b" | cut -f1)
fi

na=$(printf '%s\n' "$la" | sed '/^$/d' | wc -l | tr -d ' ')
nb=$(printf '%s\n' "$lb" | sed '/^$/d' | wc -l | tr -d ' ')
printf '旧件 deb 数：%s\n新件 deb 数：%s\n' "$na" "$nb"

# comm 要求输入**严格有序**：先把两边各自「去空行 + 排序」落到临时文件再比。
# （首版把 `printf | sed` 的结果直接喂 comm，sed 之后顺序不再保证 ⇒ comm 报
#  "input is not in sorted order" 并给出错误结果 —— 那会把「少包」漏判成一致。）
TA=$(mktemp); TB=$(mktemp)
printf '%s\n' "$la" | sed '/^$/d' | LC_ALL=C sort -u > "$TA"
printf '%s\n' "$lb" | sed '/^$/d' | LC_ALL=C sort -u > "$TB"
added=$(comm -13 "$TA" "$TB")
dropped=$(comm -23 "$TA" "$TB")
rm -f "$TA" "$TB"

fail=0
for n in $added; do
    case " $ALLOW_ADD " in *" $n "*) printf '  + %s（已登记新增）\n' "$n" ;; *) printf '  + %s  ← 未登记的新增包名\n' "$n"; fail=1 ;; esac
done
for n in $dropped; do
    case " $ALLOW_DROP " in *" $n "*) printf '  - %s（已登记移除）\n' "$n" ;; *) printf '  - %s  ← 未登记的消失包名（这正是"少带包"的样子）\n' "$n"; fail=1 ;; esac
done
[ -z "$added$dropped" ] && echo '  包名集合一致（无新增/消失）'

delta=$((nb - na)); delta=${delta#-}
if [ "$delta" -gt "$DELTA_WARN" ]; then
    printf '  ! 包数差异 %d 超过告警阈值 %d——请确认不是整批漏带/多带\n' "$delta" "$DELTA_WARN"
fi

if [ "$fail" -ne 0 ]; then
    printf '\n结论：**不可直接发布**——有未登记的包名增删。\n'
    printf '  确认无误时用环境变量登记：ALLOW_ADD="…" ALLOW_DROP="…" %s OLD NEW\n' "$(basename "$0")"
    exit 1
fi
printf '\n结论：包名集合可解释（总数 %s → %s）。\n' "$na" "$nb"
