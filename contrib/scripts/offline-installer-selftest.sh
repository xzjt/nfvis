#!/usr/bin/env bash
# 离线安装包（.run）工具链的**自校准**与产物结构校验。进 make check 的 toolcheck。
#
# 为什么自校准：这套工具的判定错了，代价是"以为闭包齐了"或"以为维护脚本没问题"——
#   两种假绿都会在气隙机器上才炸。按本仓库的口径，判定函数必须能对**已知正确**判通过、
#   对**已知错误**判失败（见 AGENTS.md 的「工具假红也是缺陷，要修并加自校准」）。
#
# 四段：
#   A 维护脚本守护自校准：造"已知正确/已知错误"的临时脚本，断言 check_maint_scripts.sh 两种结论；
#     并把守护跑到真实文件（deploy 下的维护脚本与安装器）上。
#   B 依赖闭包校验器自校准：用 dpkg-deb 造玩具 deb（a 依赖 b），断言"齐 → 自洽、缺 b → 不自洽"。
#   C0 安装清单提取器自校准（恒跑）：v3 把 PKGS_INSTALL 拆成 PKGS_COMMON/PKGS_VPP（由 pkgs_install()
#     合并输出），旧提取器在拆分后提取为空、让「安装清单里的包名都在载荷内」退化成空转假绿（R2-19③）；
#     这里对"已知正确/旧形状/已知空清单/已知缺包"四种夹具断言提取与判据，§C 再对空结果直接判 bad。
#   C 产物结构校验（可选，--run FILE 或 --dir DIR）：核对 SHA256SUMS、Packages 与 deb 一一对应、
#     闭包自洽、安装清单里的包名都在包里（缺 dpkg-deb 的宿主如实跳过闭包/版本两项）。
#
# 用法：
#   bash contrib/scripts/offline-installer-selftest.sh          # A + B + C0（CI 可跑）
#   bash contrib/scripts/offline-installer-selftest.sh --run build/nfvis-v1.1.47.run
#   bash contrib/scripts/offline-installer-selftest.sh --dir /var/tmp/nfvis-run.XXXXXX  # 已解包的载荷跑 C
set -u

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
RUN_FILE=""
DIR_ARG=""
while [ $# -gt 0 ]; do
    case "$1" in
        --run) shift; RUN_FILE=${1:-} ;;
        --dir) shift; DIR_ARG=${1:-} ;;
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

# ---------------- C0 安装清单提取器（恒跑；§C 判据的自校准） ----------------
# 由来（R2-19③）：§C 的「安装清单里的每个包名都在载荷内」原先只认 install.sh 里的
#   PKGS_INSTALL="…"，而 v3 已把清单拆成 PKGS_COMMON/PKGS_VPP（由 pkgs_install() 合并输出）——
#   抠不到任何东西时该项**静默通过**（还打印「共 1 个」），装机漏包只能到气隙机器上才炸。
# 判据口径：① 提取为空 ⇒ 失败（禁止空转）；② 条数低于下界 ⇒ 失败（提取器与清单脱节）；
#   ③ 每个包名都要能在载荷 Packages 索引里找到。
printf '\n== C0 安装清单提取器（install.sh 的 PKGS_* ⇄ 载荷）\n'

# extract_pkg_var <install.sh> <变量名>：打印该变量引号内的每个包名（一行一个）
extract_pkg_var() {
    awk -v var="$2" 'index($0, var "=\"") == 1 { f = 1 } f { print } f && /"[ \t]*$/ { exit }' "$1" |
        sed -e "s/^${2}=\"//" | tr -d '"' | tr -s ' \t' '\n' | sed '/^$/d'
}

# extract_pkgs <install.sh>：PKGS_COMMON + PKGS_VPP 并集去重（每行一个包名）；
# 两个都取不到时按 `^PKGS_[A-Z0-9_]*="` 泛化兜底（变量改名/再拆分时不静默空转）。
extract_pkgs() {
    local f="$1" v want=""
    for v in PKGS_COMMON PKGS_VPP; do
        want="$want $(extract_pkg_var "$f" "$v")"
    done
    if ! printf '%s\n' $want | grep -q .; then
        for v in $(grep -o '^PKGS_[A-Z0-9_]*=' "$f" 2>/dev/null | sed 's/=$//'); do
            want="$want $(extract_pkg_var "$f" "$v")"
        done
    fi
    printf '%s\n' $want | sed '/^$/d' | LC_ALL=C sort -u
}

# 条数下界：只是"提取器没瞎"的金丝雀（现行清单 16 个）；真正的判据是「声明过的 PKGS_* 都取到值」。
PKGS_WANT_MIN=8

# pkgs_want_check <install.sh> <debs/Packages>：0=清单可用且包名齐全（stdout ok:<条数>）；
# 1=清单提取不可用（空/过少；**禁止空转通过**）；2=有包名不在载荷内（stdout 缺口清单）。
pkgs_want_check() {
    local want n p missing=""
    want=$(extract_pkgs "$1")
    if ! printf '%s\n' $want | grep -q .; then
        printf '提取不到任何安装清单（install.sh 的 PKGS_* 提取为空）'
        return 1
    fi
    n=$(printf '%s\n' $want | sed '/^$/d' | wc -l | tr -d ' ')
    if [ "$n" -lt "$PKGS_WANT_MIN" ]; then
        printf '只提取到 %d 个包名（下界 %d）——提取器与 install.sh 的清单形状可能已脱节' "$n" "$PKGS_WANT_MIN"
        return 1
    fi
    if [ ! -f "$2" ]; then
        printf '载荷缺 debs/Packages 索引，无法核对安装清单'
        return 2
    fi
    for p in $want; do
        grep -q "^Package: ${p}$" "$2" || missing="$missing $p"
    done
    if [ -n "$missing" ]; then
        printf '安装清单有包不在载荷内：%s' "$missing"
        return 2
    fi
    printf 'ok:%d' "$n"
    return 0
}

# 夹具：已知正确（v3 形状）、旧形状（PKGS_INSTALL，v2 时代的 .run 仍要被对账）、已知错误（无 PKGS_*）
C0_V3="$TMP/c0-v3.sh"
C0_RN="$TMP/c0-old.sh"
C0_NO="$TMP/c0-none.sh"
cat > "$C0_V3" <<'EOS'
PKGS_COMMON="nfvis
docker.io chrony tcpdump curl ca-certificates"
PKGS_VPP="vpp vpp-plugin-core vpp-plugin-dpdk vpp-drivers curl"
pkgs_install() { printf '%s\n%s' "$PKGS_COMMON" "$PKGS_VPP"; }
EOS
cat > "$C0_RN" <<'EOS'
PKGS_INSTALL="nfvis vpp chrony"
EOS
cat > "$C0_NO" <<'EOS'
INSTALL_LIST="nfvis vpp"
EOS

printf 'ca-certificates\nchrony\ncurl\ndocker.io\nnfvis\ntcpdump\nvpp\nvpp-drivers\nvpp-plugin-core\nvpp-plugin-dpdk\n' > "$TMP/c0.exp"
if diff -u "$TMP/c0.exp" <(extract_pkgs "$C0_V3") > "$TMP/c0.diff" 2>&1; then
    ok "已知正确（PKGS_COMMON + PKGS_VPP）提取为并集去重（10 个包名，跨清单重复项已去重）"
else
    bad "v3 形状清单提取不对：$(tr '\n' ' ' < "$TMP/c0.diff")"
fi
if extract_pkgs "$C0_RN" | grep -qx 'vpp'; then
    ok "旧形状（PKGS_INSTALL，v2 时代的 .run）走泛化兜底仍能提取（不静默空转）"
else
    bad "旧形状（PKGS_INSTALL）提取不到——泛化兜底失效"
fi
if [ -z "$(extract_pkgs "$C0_NO")" ]; then
    ok "已知错误（无 PKGS_* 变量）提取为空——由判据判失败（下一项红-绿断言）"
else
    bad "无清单变量却提取出内容——提取器在瞎猜"
fi

printf 'Package: ca-certificates\nPackage: chrony\nPackage: curl\nPackage: docker.io\nPackage: nfvis\nPackage: tcpdump\nPackage: vpp\nPackage: vpp-plugin-core\nPackage: vpp-plugin-dpdk\nPackage: vpp-drivers\n' > "$TMP/c0.packages"
grep -v '^Package: vpp-drivers$' "$TMP/c0.packages" > "$TMP/c0.packages.missing"
c0_rc=0; c0_out=$(pkgs_want_check "$C0_V3" "$TMP/c0.packages") || c0_rc=$?
if [ "$c0_rc" -eq 0 ] && [ "$c0_out" = "ok:10" ]; then
    ok "已知正确（清单齐备）判「可用」并报出包数 10"
else
    bad "已知正确的清单被判不可用或包数不对（rc=$c0_rc，$c0_out）"
fi
c0_rc=0; c0_out=$(pkgs_want_check "$C0_V3" "$TMP/c0.packages.missing") || c0_rc=$?
if [ "$c0_rc" -eq 2 ] && printf '%s' "$c0_out" | grep -q 'vpp-drivers'; then
    ok "已知缺包（载荷缺 vpp-drivers）判「包不在载荷内」并点名"
else
    bad "缺包方向没判出来（rc=$c0_rc，$c0_out）"
fi
c0_rc=0; c0_out=$(pkgs_want_check "$C0_NO" "$TMP/c0.packages") || c0_rc=$?
if [ "$c0_rc" -eq 1 ]; then
    ok "已知错误（空清单）判「提取不可用」——旧版式空转假绿不会再发生"
else
    bad "空清单没被判失败（rc=$c0_rc，$c0_out）——§C 仍可能空转"
fi

# ---------------- C 产物结构校验（可选：--run FILE 或 --dir DIR） ----------------
check_payload() { # $1=已解包的载荷目录；判定计数走全局 PASS/FAIL
    local EX="$1" pkg_n deb_n mismatch rc judged
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
    if command -v dpkg-deb >/dev/null 2>&1; then
        if bash "$ROOT/contrib/scripts/offline-closure.sh" check "$EX/debs" > "$TMP/c4.log" 2>&1; then
            ok "依赖闭包自洽：$(sed -n 's/^  //p' "$TMP/c4.log" | head -1)"
        else
            bad "依赖闭包不自洽：$(grep '需要' "$TMP/c4.log" | head -5 | tr '\n' ' ')"
        fi
    else
        skip "依赖闭包自洽（本机没有 dpkg-deb，非 Debian 系宿主）"
    fi
    # 安装清单里的包名必须都在包里（否则安装时必然失败）。清单形状：v3 把 PKGS_INSTALL 拆成了
    # PKGS_COMMON/PKGS_VPP（pkgs_install() 合并输出）——两个都提取、并集去重；
    # **提取为空必须判失败**（R2-19③：老提取器在拆分后静默空转、还打印「共 1 个」）。
    rc=0; judged=$(pkgs_want_check "$EX/install.sh" "$EX/debs/Packages") || rc=$?
    case "$rc" in
        0) ok "安装清单里的每个包名都在载荷内（共 ${judged#ok:} 个）" ;;
        2) bad "$judged" ;;
        *) bad "包清单提取不可用（禁止空转通过）：$judged——提取器需与 install.sh 同步" ;;
    esac
    # 载荷内的 nfvis 版本与 VERSION 一致
    if command -v dpkg-deb >/dev/null 2>&1; then
        local v1 v2
        v1=$(dpkg-deb -f "$EX"/debs/nfvis_*.deb Version 2>/dev/null | head -1)
        v2=$(cat "$EX/VERSION" 2>/dev/null)
        if [ -n "$v1" ] && [ "$v1" = "$v2" ]; then
            ok "版本一致：nfvis 包 = VERSION = $v1"
        else
            bad "版本不一致：包 $v1、VERSION $v2"
        fi
    else
        skip "版本比对（本机没有 dpkg-deb）"
    fi
}

if [ -n "$RUN_FILE" ]; then
    printf '\n== C 产物结构：%s\n' "$RUN_FILE"
    [ -f "$RUN_FILE" ] || { bad "找不到产物：$RUN_FILE"; printf '\n通过 %d / 失败 %d\n' "$PASS" "$FAIL"; exit 1; }
    EX="$TMP/extract"
    if sh "$RUN_FILE" --extract "$EX" > "$TMP/c1.log" 2>&1; then
        ok "自解压头部可执行、载荷可解包"
    else
        bad "解包失败：$(cat "$TMP/c1.log")"
    fi
    check_payload "$EX"
elif [ -n "$DIR_ARG" ]; then
    printf '\n== C 产物结构（已解包的载荷目录）：%s\n' "$DIR_ARG"
    [ -d "$DIR_ARG" ] || { bad "找不到载荷目录：$DIR_ARG"; printf '\n通过 %d / 失败 %d\n' "$PASS" "$FAIL"; exit 1; }
    check_payload "$DIR_ARG"
fi

printf '\n通过 %d / 失败 %d\n' "$PASS" "$FAIL"
if [ "$FAIL" -gt 0 ]; then exit 1; fi
exit 0
