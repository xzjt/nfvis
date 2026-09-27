#!/usr/bin/env bash
# NFViS CLI 语义校验：「结果对不对」（**手动**，不在 CI / make check 中运行）
#
# 与 cli-fulltest.sh 的分工（两者互补，跑法都需真机）：
#   cli-fulltest.sh            问「命令能不能用」—— 行首出现 %/%% 即失败，
#                              **不判定输出内容是否正确**（256 条命令 197 全绿，仍可能答非所问）。
#   cli-semantic-check.sh      问「结果对不对」—— 为每条断言指定一个**独立事实源**（oracle），
#                              先取事实、再要求 CLI 与之一致；对「该看运行态」的命令，
#                              还会**改动事实源**看 CLI 是否跟着变（配置驱动的实现不会变 → 判失败）。
#
# 为什么需要它：2026-09-15 用本脚本一次跑出 4 类契约违反（决策 #84）——`show virtual-switches`
# 显示配置而非运行态、`ports`/`statistics` 静默回落到配置 dump、`show interfaces physical`
# 的 Admin 取自配置导致链路已 down 仍显示 up、`| display set` 从未实现却四处宣称可用——
# 而这些在 256 条全功能冒烟里**全是绿的**。
#
# 三种手法：
#   ① 事实源对照：CLI 输出 vs 独立 oracle（VPP / 内核 sysfs / 配置库 / libvirt / Docker）；
#   ② 扰动判别：改动事实源（在 VPP 里建一个配置中没有的 BD、把接口链路翻 down），看 CLI 是否跟随；
#   ③ round-trip：写 → 读 → 删 → 再读，验证配置通路自洽。
#
# 判定分**三档**（2026-09-26 round84 复盘后改；此前只有「通过/失败」，于是「没有证据」被算成通过）：
#   通过（✓）   —— 有正向控制且与事实源一致；
#   失败（✗）   —— 与事实源矛盾（含「一侧非空另一侧为空」）；
#   不可判定（?）—— **缺正向控制**或**无对象可对照**，单列计数、**不得计入通过**。
#   小结会打印「有 N 项不可判定」，据此可以区分「真的都对」与「没测到」。
#   这是「四个维度」缺口的补丁之一：套件量的是「命令能用」，看不见作用效果/生命周期/跨对象组合/对抗性对照。
#
# ⚠️ 自身也会出错，故必须**自校准**：oracle 取错、断言写错都会制造假红/假绿，而假绿比没有测试更危险。
#    修脚本后，请拿「已知正确」的实现确认判 PASS、拿「已知错误」的确认判 FAIL（红-绿）。
#    已知坑（都已在本脚本里规避，勿退回去）：
#      · 终端文本解析脆弱 → 候选一律走 REST（JSON），不 sed 终端输出；
#      · 输出格式会变（本脚本会随契约调整）→ 解析要有兜底并打印原始片段；
#      · 扰动必须**真的改变**事实源：用确定空闲的 BD id、把接口翻到**相反**状态，
#        并先确认「扰动前后 oracle 值不同」，否则本项应报「无从判别」而不是 PASS；
#      · 判定函数（s8_verdict / s9_verdict）刻意写成**纯函数**并被 cli-semantic-selftest.sh 抽取自校准，
#        改判定时连同自校准一起改，否则「空结果算通过」这类返祖会静默回来。
#
# 前置（缺一不可，均在 nfvis-vm 上）：
#   1) 开发态 nfvisd 已起：/tmp/nfvisd -db /tmp/nfvis-cli.db -listen 127.0.0.1:18443 \
#        -init-admin-password "Admin@12345" -allow-plaintext
#   2) /tmp/nfvis-cli 为**同版本**构建产物；口令 Admin@12345
#   3) VPP 运行中（vppctl 可用）；内核侧读 /sys/class/net；libvirt/Docker 可选（缺失则该项降级为登记）
#
# 用法：
#   bash contrib/scripts/cli-semantic-check.sh
# 退出码：有失败项 → 1（可用作发布前门槛）。**不可判定不算失败、也不算通过**，单列计数；
#        要判断「会不会是环境让本项测不到」，看小结里的不可判定条数。
#
# 副作用与恢复：脚本会临时改 VPP（建/删一个 BD、翻转某接口 admin 状态、在**开发态实例**里
# 建一个交换机并删除）。结束时会尽力恢复；仍建议随后 `systemctl restart vpp` + `restart nfvis`
# 以清掉残留拓扑（脚本会打印提醒）。
set -u

SRV=${SRV:-http://127.0.0.1:18443}
CLI_BIN=${CLI_BIN:-/tmp/nfvis-cli}
# HTTPS（产品缺省自签证书）：curl 必须能校验它，否则**发布前门槛在真实安装上跑不了**
# （2026-09-18 实测：不信任自签证书 → curl exit 60 → 拿不到 token → 断言连片假红）。
#   · 显式给证书：NFVIS_CA=/path/server.crt
#   · 缺省自动用产品路径（已装实例即开箱可用）
#   · 仅在实验室里可显式 NFVIS_INSECURE=1（= curl -k），**不默认开启**
CURL_TLS=()
case "$SRV" in
  https://*)
    if [ "${NFVIS_INSECURE:-0}" = "1" ]; then
      CURL_TLS=(-k)
    else
      CA=${NFVIS_CA:-/var/lib/nfvis/tls/server.crt}
      if [ ! -r "$CA" ]; then
        echo "✗ $SRV 是 HTTPS，但读不到服务端证书（$CA）"
        echo "  已装实例通常可直接读；否则请显式指定：NFVIS_CA=/path/to/server.crt SRV=https://… bash $0"
        echo "  （实验室里确要跳过校验可显式 NFVIS_INSECURE=1）"
        exit 1
      fi
      CURL_TLS=(--cacert "$CA")
    fi;;
esac
curl_api() { curl -s "${CURL_TLS[@]}" "$@"; }
PW=${NFVIS_PASSWORD:-Admin@12345}
PERTURB_BD=${PERTURB_BD:-5100}   # 扰动用的 BD id（须为确定空闲；脚本用完即删）
MARK=$$                           # 本次运行的唯一后缀，便于清理

cli() { "$CLI_BIN" -server "$SRV" -u admin -p "$PW" -source console -c "$1" 2>&1 | grep -v '^连接'; }

# ---------- 前置自检（缺一即明确退出，**不要让断言以「空输出」形式连片假红**）----------
if [ ! -x "$CLI_BIN" ]; then
  echo "✗ 找不到可执行的 CLI: $CLI_BIN"
  echo "  默认是 /tmp/nfvis-cli（开发态构建产物）。若产品已装到系统上，请显式指定："
  echo "      CLI_BIN=/usr/bin/nfvis-cli bash $0"
  echo "  ⚠️ 缺了它不会立刻报错，而是**所有经 CLI 的断言都退化成空输出 → 一连串假红**"
  echo "     （2026-09-16 首次发 v1.1.7 时就踩过：3 通过 / 6 失败，实际产品完全正常）。"
  exit 1
fi
if ! command -v vppctl >/dev/null 2>&1; then
  echo "✗ 找不到 vppctl：本脚本需要 VPP 作为独立事实源（oracle）"
  exit 1
fi

TOKEN=$(curl_api -X POST "$SRV/api/v1/login" -H 'Content-Type: application/json' \
        -d "{\"username\":\"admin\",\"password\":\"$PW\"}" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
if [ -z "$TOKEN" ]; then
  echo "✗ 登录失败：确认开发态 nfvisd 已在 $SRV 运行、口令为 $PW（见脚本头部前置）"
  exit 1
fi

# CLI 也必须真的能通（口令错/守护进程没起/二进制不匹配都会让后续断言假红）
selfcheck=$("$CLI_BIN" -server "$SRV" -u admin -p "$PW" -c "show version" 2>&1 | head -3)
if ! printf '%s' "$selfcheck" | grep -q "NFViS"; then
  echo "✗ CLI 无法通过 $SRV 取得输出（确认守护进程已起、口令正确、CLI 与服务端版本匹配）："
  printf '%s\n' "$selfcheck" | sed 's/^/      /'
  exit 1
fi

# 候选一律走 REST（JSON 干净），避免解析终端文本——这是本脚本最容易写错的地方
api_cands() {
  curl_api "$SRV/api/v1/cli/candidates?tokens=$1&partial=" -H "Authorization: Bearer $TOKEN" \
    | tr ',' '\n' | sed -n 's/.*"Token":"\([^"]*\)".*/\1/p' | sort
}

# ---------- oracles（独立事实源）----------
# 接口名集合：**行首无缩进**才认（VPP 的 `show interface` 把 rx/tx/drops/ip4 计数缩进续行打印，
# 那些行的第 2 列也是数字——只按「第 2 列是数字」筛会把 `drops`/`ip4` 当成接口名，
# 于是集合里混进垃圾（2026-09-26 实测））。接口行与表头都在行首，续行都是缩进的。
vpp_ifaces()  { vppctl show interface 2>/dev/null | tr -d '\r' | awk '/^[^ \t]/ && $2 ~ /^[0-9]+$/ {print $1}' | grep -v '^local0$' | sort; }
vpp_state()   { vppctl show interface 2>/dev/null | awk -v n="$1" '$1==n && $2 ~ /^[0-9]+$/ {print $3}'; }
vpp_bd_ids()  { vppctl show bridge-domain 2>/dev/null | awk 'NR>1 && $1 ~ /^[0-9]+$/ {print $1}' | sort; }
# ⚠️ vppctl 输出是 **CRLF** 行尾：值要 `tr -d '\r'` 才能做**精确比较**
#    （2026-09-22 实测：BD-Tag 取成 "vs-vnf\r"，`[ "$tg" = "$tag" ]` 恒不成立）。
vpp_bd_tag()  { vppctl show bridge-domain "$1" detail 2>/dev/null | sed -n 's/.*BD-Tag: //p' | tr -d '\r'; }
vpp_bd_tags() { for id in $(vpp_bd_ids); do vpp_bd_tag "$id"; done | grep -v '^$' | sort; }
# <bd-index> → 该 BD 的 l2fib 条数。**必须用 `verbose`**：VPP 26.06 的 `show l2fib`
# 只打印一行汇总（"L2FIB total/learned entries: N/M"），条目行只有 `verbose` 才有——
# 旧实现按非 verbose 输出的行数计，只要有表项就恒得 0 → 假红（2026-09-22 实测：
# CLI mac-table 1 条 vs 旧 oracle 0，而 VPP 汇总行本就写着 1/0）。
vpp_l2fib_count() {
  local out; out=$(vppctl show l2fib verbose 2>/dev/null)
  printf '%s\n' "$out" | awk -v bd="$1" 'NR>1 && $2 == bd {n++} END {print n+0}'
}
# <tag> → 该 BD 的 **Index**。l2fib 的 BD-Idx 列是 Index，不是 BD-ID（两者可差很大：
# 本机 vs-vnf 的 BD-ID=6252701、Index=1），故不能拿 BD-ID 去比。
vpp_bd_index_of_tag() {
  local tag="$1" id d idx
  for id in $(vpp_bd_ids); do
    [ "$(vpp_bd_tag "$id")" = "$tag" ] || continue    # 复用 vpp_bd_tag（CR 处理只有一处）
    d=$(vppctl show bridge-domain "$id" detail 2>/dev/null)
    idx=$(printf '%s\n' "$d" | awk 'NR>1 && $1 ~ /^[0-9]+$/ {print $2; exit}' | tr -d '\r')
    [ -n "$idx" ] && { echo "$idx"; return; }
  done
}
kernel_phys() { for n in /sys/class/net/*; do [ -e "$n/device" ] && basename "$n"; done | sort; }
# <BD-ID> → 该 BD 的成员口（`show bridge-domain <id> detail` 的成员表）。
# 不按表头列位解析（列宽会随版本变），而是拿**已知接口名集合**去认行：首列是接口名、次列是数字 If-idx。
# 无成员时该段整块不打印（VPP 只在有成员时才输出成员表），因此空集合是正常结果，不是解析失败。
vpp_bd_members() {
  local out names n rest
  out=$(vppctl show bridge-domain "$1" detail 2>/dev/null)
  names=$(vpp_ifaces | tr '\n' ' ')      # 用空格分隔才做得成整词匹配（换行会把「词尾」判错）
  printf '%s\n' "$out" | while read -r n rest; do
    case "$rest" in [0-9]*) case " $names " in *" $n "*) echo "$n";; esac;; esac
  done
}
# <ifname> → 该口 rx 包数（无该口/取不到 → 0）。用于「这个成员能不能产生流量」这唯一一件事。
vpp_if_rx() {
  vppctl show interface "$1" 2>/dev/null | tr -d '\r' \
    | awk '$1 == "rx" && $2 == "packets" {print $3; exit}' | grep -E '^[0-9]+$' || echo 0
}
# <ifname> → 该口的 L3 地址行（`show interface addr` 里缩进的那行），无则空。
vpp_if_l3() {
  vppctl show interface addr 2>/dev/null | tr -d '\r' | awk -v n="$1" '
    $0 !~ /^[ \t]/ { cur = ($1 == n); next }
    cur && /L3 / { print; exit }'
}
# 邻居表 → 「IP MAC 接口名」三列（`show ip neighbors` 的 Ethernet 列格式固定，用它认行）。
vpp_neighbors() {
  vppctl show ip neighbors 2>/dev/null | tr -d '\r' \
    | awk 'NF >= 5 && $4 ~ /^([0-9a-fA-F][0-9a-fA-F]:){5}[0-9a-fA-F][0-9a-fA-F]$/ {print $2, tolower($4), $5}'
}
vpp_peer_ip()  { vpp_neighbors | awk -v i="$1" '$3 == i {print $1; exit}'; }
vpp_peer_mac() { vpp_neighbors | awk -v i="$1" -v ip="$2" '$3 == i && $1 == ip {print $2; exit}'; }
# <BD-Idx> → 该 BD 的 l2fib **MAC 集合**（小写）。条数由 vpp_l2fib_count 给，这里判「学到的是不是那个对端」。
vpp_l2fib_macs() {
  vppctl show l2fib verbose 2>/dev/null | tr -d '\r' \
    | awk -v bd="$1" 'NR > 1 && $2 == bd {print tolower($1)}'
}
# CLI 的交换机列表：运行态表格（BD-ID 为数字的首列）；兼容旧配置块格式。
# 无 tag 的 BD 记为 "-" 并**保留**——它正是「运行态多出一个」的证据（曾因过滤掉它而假红）。
cli_bdnames() {
  cli "show virtual-switches" | awk '
    NR>1 && $1 ~ /^[0-9]+$/ {print ($2 == "" ? "-" : $2); next}   # 运行态表格
    /^virtual-switches /    {print $2}                            # 旧配置块格式（兜底）
  ' | sort | tr '\n' ' '
}
# 列表**行数**：与名字集合解耦（名字可能为空/重名），用于「扰动后是否多出一行」
cli_bdrows() {
  cli "show virtual-switches" | awk '
    NR>1 && $1 ~ /^[0-9]+$/ {n++; next}
    /^virtual-switches /    {n++}
    END {print n+0}'
}

PASS=0; FAIL=0; INFO=0; UNK=0
ok()   { echo "  ✓ $1"; PASS=$((PASS+1)); }
bad()  { echo "  ✗ $1"; FAIL=$((FAIL+1)); }
unk()  { echo "  ? $1"; UNK=$((UNK+1)); }
note() { echo "  · $1"; INFO=$((INFO+1)); }
hdr()  { echo; echo "===== $1"; }
cmp_sets() { # cmp_sets <标签> <期望> <实际>
  local label="$1" exp="$2" got="$3" miss=""
  for i in $exp; do case " $got " in *" $i "*) ;; *) miss="$miss $i";; esac; done
  echo "    期望: ${exp:-（空）}"; echo "    实际: ${got:-（空）}"
  if [ -z "$miss" ]; then ok "$label"; else bad "$label —— 缺:$miss"; fi
}
# 判定函数输出的三档结果 → 计数。格式 `<PASS|FAIL|UNKNOWN>:<说明>`。
judge() { # judge <项名> <判定输出>
  local name="$1" v="$2" tok="${2%%:*}" msg="${2#*:}"
  case "$tok" in
    PASS)    ok "$name —— $msg";;
    FAIL)    bad "$name —— $msg";;
    UNKNOWN) unk "$name —— $msg";;
    *)       bad "$name —— 判定函数输出异常: $v";;
  esac
}

# ---------- S8/S9 判定（纯函数：供 cli-semantic-selftest.sh 抽取做红-绿自校准）----------
# S8：<CLI 条数> <VPP l2fib 条数> <正控状态> <该对端 MAC 是否已在 l2fib>
#   正控状态：learned —— 本次确实制造了流量（发包数 > 0）且事实源可读
#             noflow  —— 有成员但流量没发出去（0 发包）
#             nopeer  —— 有可产生流量的成员，但取不到可 ping 的对端
#             nomember—— 该交换机没有能产生流量的成员
# 判据（**没有正向控制就没有通过**）：
#   缺正控 → UNKNOWN（不是 PASS——「两边都空」曾被算通过，于是没流量时永远绿）；
#   有正控 → 事实源必须真的学到该对端 MAC（学不到即失败），且与 CLI 条数一致。
s8_verdict() { # <cli_n> <fib_n> <control> <mac_learned>
  local cli_n="$1" fib_n="$2" ctl="$3" mac="$4"
  case "$ctl" in
    nopeer)   printf 'UNKNOWN:有可产生流量的成员但取不到可 ping 的对端（缺正向控制）'; return;;
    nomember) printf 'UNKNOWN:该交换机没有能产生流量的成员（缺正向控制）'; return;;
    noflow)   printf 'UNKNOWN:流量未发出（ping 0 发包），事实源无从判别'; return;;
    learned)  ;;
    *)        printf 'UNKNOWN:正控状态未知（%s）' "$ctl"; return;;
  esac
  if [ "${fib_n:-0}" -eq 0 ]; then
    printf 'FAIL:已制造流量但事实源没学到任何表项（l2fib 0 条，CLI %s 条）' "$cli_n"; return
  fi
  if [ "$mac" != "yes" ]; then
    printf 'FAIL:已制造流量、事实源有 %s 条，但不含该对端 MAC（学到的不是本次流量的对端）' "$fib_n"; return
  fi
  if [ "$cli_n" -eq "$fib_n" ]; then
    printf 'PASS:两侧一致（CLI %s 条 / VPP %s 条，正向控制已学到该对端 MAC）' "$cli_n" "$fib_n"
  else
    printf 'FAIL:条数不一致（CLI %s vs VPP %s）' "$cli_n" "$fib_n"
  fi
}
# S9：<左标签> <左集合> <右标签> <右集合>（集合是空格分隔的名字串）
#   两侧都空 → UNKNOWN（无对象可对照，**此后不再静默通过**）；
#   一侧非空另一侧为空 → FAIL；
#   两侧非空且有缺项 → FAIL；一致 → PASS。
s9_verdict() { # <l_label> <l_set> <r_label> <r_set>
  local ll="$1" ls="$2" rl="$3" rs="$4" i miss=""
  if [ -z "$ls" ] && [ -z "$rs" ]; then
    printf 'UNKNOWN:两侧都空（无对象可对照）'; return
  fi
  if [ -z "$rs" ]; then
    printf 'FAIL:%s 有对象而 %s 视图为空：%s' "$ll" "$rl" "$ls"; return
  fi
  if [ -z "$ls" ]; then
    printf 'FAIL:%s 有对象而 %s 视图为空：%s' "$rl" "$ll" "$rs"; return
  fi
  for i in $ls; do case " $rs " in *" $i "*) ;; *) miss="$miss $i";; esac; done
  if [ -n "$miss" ]; then
    printf 'FAIL:%s 有而 %s 未列出：%s' "$ll" "$rl" "$miss"
  else
    printf 'PASS:两侧一致（%s）' "$ls"
  fi
}

echo "==================== CLI 语义校验（结果对不对）===================="
echo "服务端 $SRV ｜ CLI $CLI_BIN ｜ 标记 $MARK"

# ============ 准备：被测接口（后续多项需要配置里有对象）============
IFACE=$(vpp_ifaces | head -1)
if [ -z "$IFACE" ]; then echo "✗ VPP 中无接口，无法继续"; exit 1; fi
# 记下该接口原有的 description，收尾时**还回原值**（发现 #15）：此前收尾是「删字段」，
# 于是脚本跑完配置里少了一个原本存在的描述——那叫"改回默认"，不叫恢复原值。
IFACE_DESC_ORIG=$(cli "show configuration" | awk -v n="$IFACE" '
  $0 ~ ("^interfaces " n " \\{") { inb=1; next }
  inb && /^\}/ { inb=0 }
  inb && /description/ { sub(/^[ \t]*description[ \t]*/, ""); sub(/;.*/, ""); print; exit }' | tr -d '\r')
# 该接口**在配置里原本是否存在**：不存在时，下面这条 set 会顺带建出 `interfaces <n> {}`，
# 收尾只删 description 就会留下一个空壳条目（2026-09-22 实测：连跑 5 次后基线里多出
# `interfaces bond0 { name bond0; }`）。故收尾要按「是否本次建出来的」决定删条目还是删字段。
IFACE_IN_CFG=$(cli "show configuration" | awk -v n="$IFACE" '$0 == "interfaces " n " {" {c++} END {print c+0}')
cli "configure
set interfaces $IFACE description semcheck-$MARK
commit" >/dev/null 2>&1

hdr "S1 接口候选（运行态）：set interfaces ? 应 = VPP 接口集"
cmp_sets "候选覆盖 VPP 接口" "$(vpp_ifaces | tr '\n' ' ')" "$(api_cands 'set,interfaces' | tr '\n' ' ')"

hdr "S2 管理口候选（运行态）：set system management interface ? 应 = 内核物理口"
cmp_sets "候选覆盖内核物理口" "$(kernel_phys | tr '\n' ' ')" "$(api_cands 'set,system,management,interface' | tr '\n' ' ')"

hdr "S3 虚拟交换机列表：**扰动判别**（运行态 vs 配置）"
before_n=$(vpp_bd_ids | grep -c . || true)
before_rows=$(cli_bdrows); before_cli=$(cli_bdnames)
echo "    扰动前：VPP 有 $before_n 个 BD；CLI 列出: ${before_cli:-（空）}"
vppctl create bridge-domain "$PERTURB_BD" >/dev/null 2>&1; sleep 1
after_n=$(vpp_bd_ids | grep -c . || true)
after_rows=$(cli_bdrows); after_cli=$(cli_bdnames)
echo "    扰动后：VPP 有 $after_n 个 BD（新增 1 个未写入配置的）；CLI 列出: ${after_cli:-（空）}"
if [ "$after_n" -gt "$before_n" ]; then
  if [ "$after_rows" -gt "$before_rows" ]; then ok "CLI 跟随运行态（VPP 新增 BD 后列表多出一行：$before_rows → $after_rows）"
  else bad "VPP 实际有 $after_n 个 BD，CLI 只列 $after_rows 行（扰动前 $before_rows）—— 该视图取自配置，不是运行态"; fi
else note "扰动未生效（BD $PERTURB_BD 可能已被占用），本项无从判别"; fi
vppctl create bridge-domain "$PERTURB_BD" del >/dev/null 2>&1; sleep 1

hdr "S4 契约要求「成员端口及状态/计数」：show virtual-switches <n> ports / statistics"
# 找一个 VPP 中真实存在的交换机（优先配置里也有的）
vs=$(cli_bdnames | tr ' ' '\n' | grep -vE '^-$|^$' | head -1)
if [ -z "$vs" ]; then
  cli "configure
set virtual-switches semcheck-$MARK type l2
commit" >/dev/null 2>&1
  vs="semcheck-$MARK"
  echo "    （已临时创建 $vs 作为被测对象）"
fi
echo "    对象: $vs"
for sub in ports statistics; do
  out=$(cli "show virtual-switches $vs $sub")
  echo "    --- $sub"; echo "$out" | sed 's/^/      | /' | head -6
  # 契约 §1.1：ports = 成员端口**及状态/计数**；statistics = **每端口收发计数**
  if echo "$out" | grep -qiE 'rx|tx|pkts|packets|counters|bytes'; then ok "$sub 含状态/计数字段"
  else bad "$sub 无任何状态/计数字段（应含状态与计数；疑似回落到配置 dump）"; fi
done

hdr "S5 接口「链接状态」：静态列名 + **扰动判别**（契约要求 驱动/链接状态/速率）"
hdrrow=$(cli "show interfaces physical" | head -1)
echo "    表头: $hdrrow"
if echo "$hdrrow" | grep -qiE 'driver|link|speed|驱动|链接状态|速率'; then
  ok "表头含驱动/链接状态/速率类字段"
else
  bad "表头无驱动/链接状态/速率列"
fi
s_before=$(vpp_state "$IFACE"); row_before=$(cli "show interfaces physical" | grep -E "^$IFACE" | tr -s ' ')
echo "    VPP 侧 $IFACE = $s_before"; echo "    CLI 行: ${row_before:-（无该行）}"
if [ "$s_before" = "up" ]; then flip=down; else flip=up; fi
vppctl set interface state "$IFACE" "$flip" >/dev/null 2>&1; sleep 1
s_after=$(vpp_state "$IFACE"); row_after=$(cli "show interfaces physical" | grep -E "^$IFACE" | tr -s ' ')
echo "    扰动 VPP→$flip 后: VPP = $s_after"; echo "    CLI 行: ${row_after:-（无该行）}"
if [ "$s_before" != "$s_after" ]; then
  if [ "$row_before" != "$row_after" ]; then ok "CLI 随 VPP 链路状态变化（$s_before→$s_after）"
  else bad "VPP 链路状态 $s_before→$s_after，CLI 行完全未变 —— 该列取自配置，非链路状态"; fi
  vppctl set interface state "$IFACE" "$s_before" >/dev/null 2>&1   # 恢复
else note "VPP 状态扰动无变化，本项无从判别"; fi

hdr "S6 配置回读一致性（round-trip：写→读→删→再读）"
cli "configure
set interfaces $IFACE description sem-rt-$MARK
commit" >/dev/null 2>&1
n1=$(cli "show configuration" | grep -c "sem-rt-$MARK" || true)
[ "$n1" -ge 1 ] && ok "commit 后能从配置读出" || bad "commit 后配置里读不到刚写的值"
cli "configure
delete interfaces $IFACE description
commit" >/dev/null 2>&1
n2=$(cli "show configuration" | grep -c "sem-rt-$MARK" || true)
[ "$n2" -eq 0 ] && ok "delete 后配置里消失" || bad "delete 后配置里仍在（$n2 处）"

hdr "S7 管道 display set（反推 set 语句）"
pipeout=$(cli "configure
show | display set")
if echo "$pipeout" | grep -q '未实现'; then
  bad "| display set 回「未实现」——该管道已实现，脚本口径过期"
elif echo "$pipeout" | grep -q '^set '; then
  ok "| display set 反推出 set 语句（$(echo "$pipeout" | grep -c '^set ') 行）"
elif echo "$pipeout" | grep -q '仅支持 json|xml'; then
  bad "| display set 仍是裸的「仅支持 json|xml」——管道未接线"
else
  note "| display set 输出为空或未知：$(echo "$pipeout" | head -1)"
fi

hdr "S8 MAC 表 ↔ VPP l2fib 条数（**必须带正向控制**；没流量=不可判定，不再算通过）"
# 旧判据「两边都空也算一致」= 没有证据被当成通过：本环境无流量时该项**永远绿**。
# 新判据：挑一个**有可产生流量成员**的交换机 → 制造一次穿过它的流量（ping 该口邻居表的对端）
# → 要求 l2fib **确实学到该对端 MAC**（学不到即失败）；确实没有可用成员/对端时如实标不可判定。
s8_tag=""; s8_idx=""; s8_member=""; s8_control=nomember; s8_mac=no
for t in $(cli_bdnames); do
  case "$t" in -|"") continue;; esac
  i=$(vpp_bd_index_of_tag "$t"); [ -n "$i" ] || continue
  s8_tag="$t"; s8_idx="$i"
  for m in $(vpp_bd_members "$i"); do
    if [ "$(vpp_if_rx "$m")" -gt 0 ] 2>/dev/null; then s8_member="$m"; break; fi
  done
  [ -n "$s8_member" ] && break
done
echo "    候选交换机: ${s8_tag:-（无）} ｜ 成员: ${s8_idx:+$(vpp_bd_members "$s8_idx" | tr '\n' ' ')}｜ 有 rx 计数的成员: ${s8_member:-（无）}"
if [ -z "$s8_idx" ]; then
  s8_cli_n=0; s8_fib_n=0
else
  out=$(cli "show virtual-switches $s8_tag mac-table")
  if echo "$out" | grep -q 'MAC 表为空'; then s8_cli_n=0
  else s8_cli_n=$(echo "$out" | awk 'NR>1 && $1 ~ /:/ {n++} END {print n+0}'); fi
  s8_fib_n=$(vpp_l2fib_count "$s8_idx")
  echo "    CLI mac-table 条数=$s8_cli_n；VPP l2fib 条数=$s8_fib_n（$s8_tag index=${s8_idx:-未在 VPP 中找到}）"
fi
if [ -n "$s8_member" ]; then
  # 对端：该成员口邻居表里的地址（ARP 已解析才可能在 l2fib 里被认出来）。
  # 流量源优先取**该 BD 里带 L3 地址的口**（BVI/网关）——从它 ping 对端，帧经 BD 转发到成员口，
  # 对端的回复从成员口进来 ⇒ L2 学习必然发生；这正是「正向控制」。
  s8_peer=$(vpp_peer_ip "$s8_member")
  s8_src=""
  for m in $(vpp_bd_members "$s8_idx"); do
    [ -n "$(vpp_if_l3 "$m")" ] && { s8_src="$m"; break; }
  done
  if [ -z "$s8_peer" ]; then
    s8_control=nopeer
    echo "    （成员 $s8_member 无邻居表项，取不到可 ping 的对端）"
  else
    if [ -n "$s8_src" ]; then
      s8_ping=$(vppctl ping "$s8_peer" source "$s8_src" repeat 5 2>&1 | tr -d '\r')
    else
      s8_ping=$(vppctl ping "$s8_peer" repeat 5 2>&1 | tr -d '\r')
    fi
    s8_sent=$(printf '%s' "$s8_ping" | sed -n 's/.*Statistics: *\([0-9][0-9]*\) sent.*/\1/p')
    echo "    制造流量: ping $s8_peer${s8_src:+ source $s8_src} → $(printf '%s' "$s8_ping" | grep -E 'Statistics:' | head -1)"
    if [ "${s8_sent:-0}" -gt 0 ]; then
      s8_control=learned
      s8_peer_mac=$(vpp_peer_mac "$s8_member" "$s8_peer")
      s8_fib_n=$(vpp_l2fib_count "$s8_idx")          # 复测事实源（学习发生在本次流量之后）
      if [ -n "$s8_peer_mac" ] && vpp_l2fib_macs "$s8_idx" | grep -qx "$s8_peer_mac"; then s8_mac=yes; fi
      echo "    正控复核: 对端 MAC=${s8_peer_mac:-（邻居表取不到）} ｜ l2fib 现有 $(vpp_l2fib_macs "$s8_idx" | tr '\n' ' ')"
      out=$(cli "show virtual-switches $s8_tag mac-table")
      if echo "$out" | grep -q 'MAC 表为空'; then s8_cli_n=0
      else s8_cli_n=$(echo "$out" | awk 'NR>1 && $1 ~ /:/ {n++} END {print n+0}'); fi
      echo "    正控后 CLI mac-table 条数=$s8_cli_n"
    else
      s8_control=noflow
    fi
  fi
fi
judge "S8 MAC 表 ↔ l2fib" "$(s8_verdict "${s8_cli_n:-0}" "${s8_fib_n:-0}" "$s8_control" "$s8_mac")"

hdr "S9 运行态对象是否被视图反映（**计入判定**：一侧非空另一侧为空即失败）"
# 旧口径「诊断，不计失败」+ 两侧都空 → 静默通过：**没有对象时永远绿**，于是「视图根本没接运行态」
# 这类缺陷在空环境里一次也测不出来。新口径按 s9_verdict 三档判（两侧都空 = 无对象可对照 = 不可判定）。
libv=$(virsh list --all 2>/dev/null | awk 'NR>2 && $2!="" {print $2}' | tr '\n' ' ')
# `-a` 必须带上：产品把「已声明但未运行」的容器如实列成 exited（absent 同理），而 VM 侧 oracle
# 用的就是 `virsh list --all`（含关机域）——只列运行中的容器与「全部声明」比较是**不对称**的，
# 任何 exited 容器都会变成假红（round85 实测：套件留下的 cli-ct2 exited 让 S9 判失败，而 CLI
# 与 `docker ps -a` 完全一致）。oracle 取错事实与实现出错一样会毁掉信任。
dockerps=$(docker ps -a --format '{{.Names}}' 2>/dev/null | tr '\n' ' ')
# CLI 视图第一行是表头，必须跳过（否则 "Name" 会被当成对象）；「（无容器）」这类整句按空处理。
clivm=$(cli "show virtual-machine-functions" | awk 'NR>1 && $1 !~ /^（/ {print $1}' | tr '\n' ' ')
clict=$(cli "show container-functions" | awk 'NR>1 && $1 !~ /^（/ {print $1}' | tr '\n' ' ')
echo "    libvirt 域: ${libv:-（无）} ｜ CLI VM 列表: ${clivm:-（空）}"
echo "    Docker 容器: ${dockerps:-（无）} ｜ CLI 容器列表: ${clict:-（空）}"
judge "S9 虚拟机列表（libvirt ↔ CLI 视图）" "$(s9_verdict 'libvirt 域' "$libv" 'CLI VM 视图' "$clivm")"
judge "S9 容器列表（Docker ↔ CLI 视图）" "$(s9_verdict 'Docker 容器' "$dockerps" 'CLI 容器视图' "$clict")"

# ============ 清理本脚本创建的对象 ============
# 接口：先看**条目本身**是不是本次建出来的——是就整条删掉（发现 #15 的同类：收尾只删字段
# 会留下空壳条目）；否则描述**还回原值**（发现 #15），原本就没有描述才删字段。
if [ "${IFACE_IN_CFG:-1}" = "0" ]; then
  cli "configure
delete interfaces $IFACE
commit" >/dev/null 2>&1
  echo "· 接口 $IFACE 原本不在配置中，已删除本次写入的条目"
elif [ -n "${IFACE_DESC_ORIG:-}" ] && [ "$IFACE_DESC_ORIG" != "semcheck-$MARK" ] && [ "$IFACE_DESC_ORIG" != "sem-rt-$MARK" ]; then
  cli "configure
set interfaces $IFACE description $IFACE_DESC_ORIG
commit" >/dev/null 2>&1
  echo "· 已恢复接口 $IFACE 的原有描述（$IFACE_DESC_ORIG）"
else
  cli "configure
delete interfaces $IFACE description
commit" >/dev/null 2>&1
  echo "· 接口 $IFACE 原本没有描述，已清除本次写入"
fi
if [ "${vs:-}" = "semcheck-$MARK" ]; then
  cli "configure
delete virtual-switches semcheck-$MARK
commit" >/dev/null 2>&1
fi
vppctl create bridge-domain "$PERTURB_BD" del >/dev/null 2>&1

echo
echo "==================== 合计：通过 $PASS / 失败 $FAIL / 不可判定 $UNK / 登记 $INFO ===================="
if [ "$UNK" -gt 0 ]; then
  echo "⚠️ 有 $UNK 项**不可判定**（缺正向控制或无对象可对照）——它们**没有**计入通过；"
  echo "   要分清「真的都对」与「这一轮没测到」：需要流量的项请在有业务流量/有对端的拓扑上复跑。"
fi
echo "（修复/复核后请对比上一次结果；失败项需人工判读契约与 oracle 后再改实现或改断言）"
if [ "$FAIL" -gt 0 ]; then
  echo "⚠️ 有失败项：若为扰动/环境所致，请先确认 oracle 侧确实变化（本脚本已尽量自证）"
  echo "⚠️ 建议随后: systemctl restart vpp && systemctl restart nfvis（清残留拓扑）"
  exit 1
fi
echo "建议随后: systemctl restart vpp && systemctl restart nfvis（清残留拓扑）"
