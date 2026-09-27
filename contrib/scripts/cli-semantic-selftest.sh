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
#
# 扩展（2026-09-26，round84 复盘的「测试盲区」）：S8/S9 的**判定函数**也纳入自校准。
# 这两项此前是**假绿来源**——S8「两边都空也算一致」、S9「诊断，不计失败」，于是环境里没对象/没流量时
# 永远绿，「没有证据」被当成「通过」。新判据把结果分三档（PASS / FAIL / UNKNOWN），
# 于是自校准必须钉住两件事：① 空结果**不得**判 PASS（旧行为必须报 ✗）；
# ② 一侧非空另一侧为空**必须**判 FAIL。改判定后请照下面两条手工验证一次：
#   · 把 s8_verdict 的 nomember/nopeer 分支改回「printf PASS」（旧行为），本脚本必须报 ✗
#   · 把 s9_verdict 的两侧都空分支改回「printf PASS」（旧行为），本脚本必须报 ✗
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

ORACLES=$(for f in vpp_bd_ids vpp_bd_tag vpp_l2fib_count vpp_bd_index_of_tag vpp_ifaces vpp_bd_members s8_verdict s9_verdict; do
  body=$(extract "$f")
  [ -z "$body" ] && { echo "✗ 抽不到函数 $f（脚本改名了？）" >&2; exit 1; }
  printf '%s\n' "$body"
done)
# shellcheck disable=SC1090
eval "$ORACLES"

CR=$'\r'
# ---- 桩 vppctl：按子命令返回各用例设定的合成输出 ----
STUB_BD=""; STUB_BD_DETAIL=""; STUB_L2FIB=""; STUB_IFACES=""
vppctl() {
  case "$*" in
    "show bridge-domain")        printf '%s\n' "$STUB_BD" ;;
    "show bridge-domain "*)      printf '%s\n' "$STUB_BD_DETAIL" ;;
    "show l2fib verbose")        printf '%s\n' "$STUB_L2FIB" ;;
    "show interface")            printf '%s\n' "$STUB_IFACES" ;;
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

echo "— BD 成员表：首列接口名 + 次列数字才算成员行（无成员时段落整块不打印）—"
# 成员行的判据是「接口名在 VPP 接口集合里」——表头列宽会随版本变，接口名集合才是稳定锚点。
STUB_IFACES="              Name               Idx    State  MTU (L3/IP4/IP6/MPLS)     Counter          Count     ${CR}
ens192                            1      up          9000/0/0/0     rx packets                   189${CR}
ens224                            2      up          9000/0/0/0     rx packets                   579${CR}
vh-vnf-dhcp-eth0                  3      up          9000/0/0/0     rx packets                   909${CR}
local0                            0      down          0/0/0/0     ${CR}"
STUB_BD_DETAIL="$STUB_BD${CR}
           Interface           If-idx ISN  SHG  BVI  TxFlood        VLAN-Tag-Rewrite       ${CR}
            ens192               1     1    0    -      *                 none             ${CR}
            vh-vnf-dhcp-eth0     3     1    0    -      *                 none             ${CR}

  BD-Tag: vs-vnf${CR}"
eq "两个成员 → 取到 ens192 与 vhost 口（表头行不算成员）" "ens192 vh-vnf-dhcp-eth0" "$(vpp_bd_members 1 | tr '\n' ' ' | sed 's/ $//')"
eq "vpp_ifaces 排除 local0、保留 vhost 口" "ens192 ens224 vh-vnf-dhcp-eth0" "$(vpp_ifaces | tr '\n' ' ' | sed 's/ $//')"
STUB_BD_DETAIL="$STUB_BD${CR}
           Interface           If-idx ISN  SHG  BVI  TxFlood        VLAN-Tag-Rewrite       ${CR}
            ens999               9     1    0    -      *                 none             ${CR}

  BD-Tag: vs-vnf${CR}"
eq "成员行里的名字不在 VPP 接口集合 → 不认（防表头/垃圾行误判）" "" "$(vpp_bd_members 1)"
STUB_BD_DETAIL="$STUB_BD${CR}

  BD-Tag: vs-l2${CR}"
eq "无成员（成员表整块不打印）→ 空集合，不报错" "" "$(vpp_bd_members 1)"

echo "— 接口名集合：只认行首无缩进的行（计数续行是缩进的，第 2 列同样是数字）—"
# VPP 把 rx/tx/drops/ip4 计数续行缩进打印，那些行的第 2 列也是数字——只按「第 2 列是数字」筛，
# 集合里会混进 drops/ip4 这类垃圾（2026-09-26 真机实测：S1 因此把 drops/ip4/ip6 当成「期望的接口」→ 假红）。
STUB_IFACES="              Name               Idx    State  MTU (L3/IP4/IP6/MPLS)     Counter          Count     ${CR}
ens192                            1      up          9000/0/0/0     rx packets                   189${CR}
                                                                   rx bytes                   19271${CR}
                                                                   drops                        189${CR}
                                                                   ip4                           85${CR}
ens224                            2      up          9000/0/0/0     tx packets                   400${CR}
                                                                   ip4                          438${CR}
vh-vnf-dhcp-eth0                  3      up          9000/0/0/0     rx packets                   909${CR}
                                                                   ip4                          634${CR}
local0                            0      down          0/0/0/0     ${CR}"
eq "接口名只取行首无缩进的行（缩进的 drops/ip4 续行不算）" "ens192 ens224 vh-vnf-dhcp-eth0" "$(vpp_ifaces | tr '\n' ' ' | sed 's/ $//')"
STUB_BD_DETAIL="$STUB_BD${CR}
           Interface           If-idx ISN  SHG  BVI  TxFlood        VLAN-Tag-Rewrite       ${CR}
            ens192               1     1    0    -      *                 none             ${CR}
            vh-vnf-dhcp-eth0     3     1    0    -      *                 none             ${CR}

  BD-Tag: vs-vnf${CR}"
eq "同一份输出下 BD 成员判断不受计数续行污染" "ens192 vh-vnf-dhcp-eth0" "$(vpp_bd_members 1 | tr '\n' ' ' | sed 's/ $//')"

echo "— S8 判定：空结果不再算通过；有正控才判，且必须真学到该对端 MAC —"
eqv() { # eqv <期望档> <说明> <判定输出>
  local want="$1" desc="$2" got="${3%%:*}" msg="${3#*:}"
  if [ "$got" = "$want" ]; then printf '  ✓ %-52s → %s（%s）\n' "$desc" "$got" "$msg"
  else printf '  ✗ %-52s → 期望 %s 实际 %s（%s）\n' "$desc" "$want" "$got" "$msg"; RC=1; fi
}
eqv UNKNOWN "两侧都空、无正控 → 不可判定（旧行为此处判 PASS）" "$(s8_verdict 0 0 nomember no)"
eqv UNKNOWN "有成员但无可 ping 对端 → 不可判定" "$(s8_verdict 0 0 nopeer no)"
eqv UNKNOWN "ping 0 发包 → 不可判定（流量没发出去）" "$(s8_verdict 0 0 noflow no)"
eqv PASS    "有正控、条数一致、该对端 MAC 已学到 → 通过" "$(s8_verdict 3 3 learned yes)"
eqv FAIL    "有正控但事实源 0 条（学不到）→ 失败" "$(s8_verdict 0 0 learned no)"
eqv FAIL    "有正控但学到的不是该对端 MAC → 失败" "$(s8_verdict 2 2 learned no)"
eqv FAIL    "有正控但条数不一致 → 失败" "$(s8_verdict 1 4 learned yes)"

echo "— S9 判定：一侧非空另一侧为空即失败；两侧都空=不可判定 —"
eqv UNKNOWN "两侧都空 → 不可判定（旧行为此处静默通过）" "$(s9_verdict 'libvirt 域' '' 'CLI 视图' '')"
eqv FAIL    "事实源有对象、CLI 视图为空 → 失败" "$(s9_verdict 'libvirt 域' 'vnf-a vnf-b' 'CLI 视图' '')"
eqv FAIL    "反向：CLI 有对象、事实源为空 → 失败" "$(s9_verdict 'Docker 容器' '' 'CLI 视图' 'ct-a')"
eqv PASS    "两侧非空且一致（顺序不同也算一致）→ 通过" "$(s9_verdict 'libvirt 域' 'vnf-a vnf-b' 'CLI 视图' 'vnf-b vnf-a')"
eqv FAIL    "两侧非空但 CLI 少一个 → 失败（并点名缺谁）" "$(s9_verdict 'libvirt 域' 'vnf-a vnf-b' 'CLI 视图' 'vnf-a')"

if [ $RC -eq 0 ]; then echo "全部符合预期"; else echo "有不符合预期的用例"; fi
exit $RC