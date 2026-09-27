#!/usr/bin/env bash
# 离线安装包（.run）工具链的**自校准**与产物结构校验。进 make check 的 toolcheck。
#
# 为什么自校准：这套工具的判定错了，代价是"以为闭包齐了"或"以为维护脚本没问题"——
#   两种假绿都会在气隙机器上才炸。按本仓库的口径，判定函数必须能对**已知正确**判通过、
#   对**已知错误**判失败（见 AGENTS.md 的「工具假红也是缺陷，要修并加自校准」）。
#
# 三段：
#   A 维护脚本守护自校准：造"已知正确/已知错误"的临时脚本，断言 check_maint_scripts.sh 两种结论；
#     并把守护跑到真实文件（deploy 下的维护脚本与安装器）上。
#   B 依赖闭包校验器自校准：用 dpkg-deb 造玩具 deb（a 依赖 b），断言"齐 → 自洽、缺 b → 不自洽"。
#   C 产物结构校验（可选，--run FILE）：解包真实 .run，逐项核对 SHA256SUMS、Packages 与 deb
#     一一对应、闭包自洽、安装清单里的包名都在包里。
#
# 用法：
#   bash contrib/scripts/offline-installer-selftest.sh              # A + B（CI 可跑）
#   bash contrib/scripts/offline-installer-selftest.sh --run build/nfvis-v1.1.47.run
set -u

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
RUN_FILE=""
while [ $# -gt 0 ]; do
    case "$1" in
        --run) shift; RUN_FILE=${1:-} ;;
        *) printf '未知参数：%s\n' "$1" >&2; exit 2 ;;
    esac
    shift
done

PASS=0; FAIL=0
ok() { printf '  [通过] %s\n' "$1"; PASS=$((PASS + 1)); }
bad() { printf '  [失败] %s\n' "$1"; FAIL=$((FAIL + 1)); }
skip() { printf '  [跳过] %s\n' "$1"; }

# ---------------- A 维护脚本守护 ----------------
printf '\n== A 维护脚本守护（顶层 exit 之后不得有可执行语句）\n'
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

GOOD="$TMP/good.sh"
BAD="$TMP/bad.sh"
printf '#!/bin/sh\nset -e\necho ok\nexit 0\n' > "$GOOD"
printf '#!/bin/sh\nset -e\necho ok\nexit 0\necho 这条永远不会执行\n' > "$BAD"

if bash "$ROOT/contrib/scripts/check_maint_scripts.sh" "$GOOD" > "$TMP/a1.log" 2>&1; then
    ok "已知正确（exit 在末尾）判通过"
else
    bad "已知正确的脚本被判违规——守护误报（日志：$TMP/a1.log）"
fi
if bash "$ROOT/contrib/scripts/check_maint_scripts.sh" "$BAD" > "$TMP/a2.log" 2>&1; then
    bad "已知错误（exit 之后还有语句）**没**被抓到——守护形同虚设"
else
    if grep -q '永远不会执行' "$TMP/a2.log"; then
        ok "已知错误（exit 之后还有语句）被判违规并指出行号"
    else
        bad "判了违规但没指出位置（日志：$TMP/a2.log）"
    fi
fi
if bash "$ROOT/contrib/scripts/check_maint_scripts.sh" > "$TMP/a3.log" 2>&1; then
    ok "真实维护脚本（deploy 下）全部通过：$(sed -n 's/^维护脚本检查：//p' "$TMP/a3.log")"
else
    bad "真实维护脚本被判违规：$(cat "$TMP/a3.log")"
fi

# ---------------- B 依赖闭包校验器 ----------------
printf '\n== B 依赖闭包校验器（集合自洽性）\n'
if ! command -v dpkg-deb >/dev/null 2>&1; then
    skip "没有 dpkg-deb（非 Debian 系宿主），闭包校验器自校准跳过"
else
    mkdir -p "$TMP/debs"
    makedeb() { # $1=包名 $2=Depends（可空）
        # 逐条赋值：同一条 local 里"赋值后再引用"在 bash 下会撞 set -u
        local name="$1"
        local dep="$2"
        local d="$TMP/build/$name"
        mkdir -p "$d/DEBIAN"
        {
            printf 'Package: %s\nVersion: 1.0\nArchitecture: all\n' "$name"
            printf 'Maintainer: selftest <selftest@example.invalid>\n'
            [ -n "$dep" ] && printf 'Depends: %s\n' "$dep"
            printf 'Description: 自校准玩具包\n'
        } > "$d/DEBIAN/control"
        dpkg-deb --root-owner-group --build "$d" "$TMP/debs/${name}_1.0_all.deb" >/dev/null 2>&1 ||
            dpkg-deb --build "$d" "$TMP/debs/${name}_1.0_all.deb" >/dev/null
    }
    makedeb toy-b ""
    makedeb toy-a "toy-b (>= 1.0)"
    if bash "$ROOT/contrib/scripts/offline-closure.sh" check "$TMP/debs" > "$TMP/b1.log" 2>&1; then
        ok "已知自洽（a 依赖 b，b 在集合内）判自洽"
    else
        bad "已知自洽的集合被判不自洽——校验器误报：$(cat "$TMP/b1.log")"
    fi
    rm -f "$TMP/debs/toy-b_1.0_all.deb"
    if bash "$ROOT/contrib/scripts/offline-closure.sh" check "$TMP/debs" > "$TMP/b2.log" 2>&1; then
        bad "缺包（删掉 toy-b）**没**被判出来——离线漏包将从这里溜过去"
    else
        if grep -q '需要 toy-b' "$TMP/b2.log"; then
            ok "已知有缺口（缺 toy-b）被判不自洽，并指出「谁需要谁」"
        else
            bad "判了不自洽但没指出缺口（日志：$TMP/b2.log）"
        fi
    fi
fi

# ---------------- C 产物结构校验（可选） ----------------
if [ -n "$RUN_FILE" ]; then
    printf '\n== C 产物结构：%s\n' "$RUN_FILE"
    [ -f "$RUN_FILE" ] || { bad "找不到产物：$RUN_FILE"; printf '\n通过 %d / 失败 %d\n' "$PASS" "$FAIL"; exit 1; }
    EX="$TMP/extract"
    if sh "$RUN_FILE" --extract "$EX" > "$TMP/c1.log" 2>&1; then
        ok "自解压头部可执行、载荷可解包"
    else
        bad "解包失败：$(cat "$TMP/c1.log")"
    fi
    if (cd "$EX" && sha256sum -c --quiet SHA256SUMS) 2> "$TMP/c2.log"; then
        ok "SHA256SUMS：载荷逐文件一致"
    else
        bad "载荷完整性失败：$(cat "$TMP/c2.log")"
    fi
    # Packages 索引与 deb 一一对应（数量、大小、sha256）
    if [ -f "$EX/debs/Packages" ]; then
        pkg_n=$(grep -c '^Package: ' "$EX/debs/Packages")
        deb_n=$(find "$EX/debs" -maxdepth 1 -name '*.deb' | wc -l)
        if [ "$pkg_n" = "$deb_n" ]; then
            ok "本地源索引：Packages 条目数 $pkg_n 与 deb 数一致"
        else
            bad "Packages 条目数 $pkg_n ≠ deb 数 $deb_n"
        fi
        mismatch=$(awk '/^SHA256: / { print $2 }' "$EX/debs/Packages" | sort > "$TMP/c3a"
            find "$EX/debs" -name '*.deb' -exec sha256sum {} + | awk '{ print $1 }' | sort > "$TMP/c3b"
            diff "$TMP/c3a" "$TMP/c3b" | wc -l)
        if [ "$mismatch" = 0 ]; then
            ok "索引内的 sha256 与实际 deb 逐一一致"
        else
            bad "索引内的 sha256 与 deb 不一致（差异行 $mismatch）"
        fi
    else
        bad "载荷缺 debs/Packages 索引"
    fi
    if bash "$ROOT/contrib/scripts/offline-closure.sh" check "$EX/debs" > "$TMP/c4.log" 2>&1; then
        ok "依赖闭包自洽：$(sed -n 's/^  //p' "$TMP/c4.log" | head -1)"
    else
        bad "依赖闭包不自洽：$(grep '需要' "$TMP/c4.log" | head -5 | tr '\n' ' ')"
    fi
    # 安装清单里的包名必须都在包里（否则安装时必然失败）
    want=$(sed -n 's/^PKGS_INSTALL="//; /^[a-z]/p' "$EX/install.sh" | tr '\n' ' ' | tr -s ' ')
    missing=""
    for p in $want; do
        grep -q "^Package: ${p}$" "$EX/debs/Packages" || missing="$missing $p"
    done
    if [ -z "$missing" ]; then
        ok "安装清单里的每个包名都在载荷内（共 $(printf '%s\n' $want | wc -l) 个）"
    else
        bad "安装清单有包不在载荷内：$missing"
    fi
    # 载荷内的 nfvis 版本与 VERSION 一致
    v1=$(dpkg-deb -f "$EX"/debs/nfvis_*.deb Version 2>/dev/null | head -1)
    v2=$(cat "$EX/VERSION" 2>/dev/null)
    if [ -n "$v1" ] && [ "$v1" = "$v2" ]; then
        ok "版本一致：nfvis 包 = VERSION = $v1"
    else
        bad "版本不一致：包 $v1、VERSION $v2"
    fi
fi

printf '\n通过 %d / 失败 %d\n' "$PASS" "$FAIL"
if [ "$FAIL" -gt 0 ]; then exit 1; fi
exit 0
