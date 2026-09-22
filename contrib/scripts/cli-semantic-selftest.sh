#!/usr/bin/env bash
# 语义校验 oracle 的自校准（「已知正确 → 对、已知错误 → 错」）。
#
# 由来：2026-09-22 复跑发现 S8（MAC 表 ↔ VPP l2fib 条数）**假红**——CLI 报 1 条、
# oracle 报 0，而 VPP 汇总行本就写着 "total/learned entries: 1/0"。两处成因都在 oracle 侧：
#   ① `vppctl show l2fib`（非 verbose）**只打印一行汇总**，条目行只有 `verbose` 才有
#      → 旧实现按非 verbose 输出的行数计，只要有表项就恒得 0；
#   ② `vppctl` 输出是 **CRLF** 行尾 → 精确比较（tag 名）必须先剥 `\r`。
# 这类「工具自身出错制造的假红」比假绿更伤信任，故与 cli-fulltest-selftest.sh 同法纳入 make check。
#
# 手法：把**脚本里真正在用的** oracle 函数抽出来（不另写一份，否则测的不是实现），
#       用桩 vppctl 喂合成的 VPP 输出。不需要真机、不需要 VPP。
# 用法：bash contrib/scripts/cli-semantic-selftest.sh
# 红-绿验证本自校准自身：SEM_SCRIPT=<改了 oracle 的副本> bash contrib/scripts/cli-semantic-selftest.sh
#   —— 喂「旧实现」必须报 ✗（证明这些断言真的在判别，不是恒绿）。
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
SCRIPT=${SEM_SCRIPT:-$HERE/cli-semantic-check.sh}

# 按函数名抽取定义：单行 `f()  { …; }` 与多行 `f() { … \n }` 都要能取到。
# 注意多行函数体内含 `}`（awk 的 {n+0}），故只认**行首**的 `}` 为结束。
extract() {
  awk -v fn="$1" '
    $0 ~ "^" fn "\\(\\)" { inf = 1; if ($0 ~ /\}[ \t]*$/) inf = 0; print; next }
    inf { print; if ($0 ~ /^\}/) inf = 0 }
  ' "$SCRIPT"
}

ORACLES=$(for f in vpp_bd_ids vpp_bd_tag vpp_l2fib_count vpp_bd_index_of_tag; do
  body=$(extract "$f")
  [ -z "$body" ] && { echo "✗ 抽不到函数 $f（脚本改名了？）" >&2; exit 1; }
  printf '%s\n' "$body"
done)
# shellcheck disable=SC1090
eval "$ORACLES"

CR=$'\r'
# ---- 桩 vppctl：按子命令返回各用例设定的合成输出 ----
STUB_BD=""; STUB_BD_DETAIL=""; STUB_L2FIB=""
vppctl() {
  case "$*" in
    "show bridge-domain")        printf '%s\n' "$STUB_BD" ;;
    "show bridge-domain "*)      printf '%s\n' "$STUB_BD_DETAIL" ;;
    "show l2fib verbose")        printf '%s\n' "$STUB_L2FIB" ;;
    *)                           printf '' ;;
  esac
}

RC=0
# 让 `\r` 可见：CR 差异两边打印出来一模一样，藏证据（正是本文件要防的那类陷阱）。
show() { printf '%s' "$1" | sed 's/\r/\\r/g'; }
eq() { # eq <说明> <期望> <实际>
  if [ "$2" = "$3" ]; then printf '  ✓ %-52s → %s\n' "$1" "$(show "$3")"
  else printf '  ✗ %-52s → 期望 [%s] 实际 [%s]\n' "$1" "$(show "$2")" "$(show "$3")"; RC=1; fi
}

echo "— l2fib 条数：必须读 verbose 的条目行（旧实现恒得 0）—"
STUB_L2FIB="    Mac-Address     BD-Idx If-Idx BSN-ISN Age(min) static filter bvi         Interface-Name        ${CR}
 b0:b0:00:00:00:00    1      5      0/0      no      *      -     *               bvi0              ${CR}
L2FIB total/learned entries: 1/0  Last scan time: 5.4e-3sec  Learn limit: 16777216${CR}"
eq "1 条表项 → 1（非 0；旧实现此处为 0 → 假红）" 1 "$(vpp_l2fib_count 1)"

STUB_L2FIB="    Mac-Address     BD-Idx If-Idx BSN-ISN Age(min) static filter bvi         Interface-Name        ${CR}
 b0:b0:00:00:00:00    1      5      0/0      no      *      -     *               bvi0              ${CR}
 02:00:00:00:00:99    1      5      0/0      no      -      -     -               bvi0              ${CR}
L2FIB total/learned entries: 2/0  Last scan time: 5.4e-3sec  Learn limit: 16777216${CR}"
eq "2 条表项 → 2（跟随事实源变化）" 2 "$(vpp_l2fib_count 1)"

STUB_L2FIB="    Mac-Address     BD-Idx If-Idx BSN-ISN Age(min) static filter bvi         Interface-Name        ${CR}
L2FIB total/learned entries: 0/0  Last scan time: 5.4e-3sec  Learn limit: 16777216${CR}"
eq "空表 → 0" 0 "$(vpp_l2fib_count 1)"

STUB_L2FIB="    Mac-Address     BD-Idx If-Idx BSN-ISN Age(min) static filter bvi         Interface-Name        ${CR}
 aa:aa:aa:aa:aa:01    7      5      0/0      no      *      -     *               bvi0              ${CR}
 aa:aa:aa:aa:aa:02    7      5      0/0      no      *      -     *               bvi0              ${CR}
L2FIB total/learned entries: 2/0  Last scan time: 5.4e-3sec  Learn limit: 16777216${CR}"
eq "表项全在别的 BD（7）→ 查 BD 1 得 0（按 BD 过滤）" 0 "$(vpp_l2fib_count 1)"
eq "同一输入查 BD 7 → 2" 2 "$(vpp_l2fib_count 7)"

echo "— BD tag → Index：CRLF 必须剥掉，且取的是 Index 不是 BD-ID —"
# 本机实测形状：BD-ID=6252701、Index=1（两者可差很大）；l2fib 的 BD-Idx 是 Index。
STUB_BD="  BD-ID   Index   BSN  Age(min)  Learning  U-Forwrd   UU-Flood   Flooding  ARP-Term  arp-ufwd Learn-co Learn-li   BVI-Intf ${CR}
 6252701    1      3     off        on        on       flood        on       off       off        0    16777216     bvi0   ${CR}"
STUB_BD_DETAIL="$STUB_BD${CR}  BD-Tag: vs-vnf${CR}"
eq "CRLF 的 BD-Tag: vs-vnf → 取到 Index 1（不剥 \\r 则恒空）" 1 "$(vpp_bd_index_of_tag vs-vnf)"
eq "tag 不存在 → 空" "" "$(vpp_bd_index_of_tag no-such-vs)"
eq "vpp_bd_tag 剥 CR 后可精确比较" "vs-vnf" "$(vpp_bd_tag 6252701)"

STUB_BD=""
STUB_BD_DETAIL=""
eq "无 BD → 空（不误报）" "" "$(vpp_bd_index_of_tag vs-vnf)"

if [ $RC -eq 0 ]; then echo "全部符合预期"; else echo "有不符合预期的用例"; fi
exit $RC