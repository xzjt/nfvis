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
# 另有 S10「配置编辑锁语义」（决策 #317/#318）：它量的是**两个会话之间**的排他与接管——
# 单条 CLI 命令是一次性会话（收尾即释放），表达不了，故经 REST 同时持两个 token，
# 用「HTTP 状态码 + 响应体 + 会话视图」三方对照，且**每项先立正向控制**再判结果
# （dirty 严格排他 / 干净锁可接管 / 被接管者 ErrLockLost / 登出只清本会话）。
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
#   4) 「不留不可判定」要求现场有对象与流量（S8 需交换机里有真的在收发帧的成员，S9 需 VM/容器对象各一）：
#        bash contrib/scripts/cli-semantic-fixture.sh up    # 造现场（见其头部；用完 down 清理）
#      缺 S8 的正控现场时该项会**如实报不可判定**（不会静默变绿）；脚本会在报告里提示这条命令。
#
# 用法：
#   bash contrib/scripts/cli-semantic-check.sh
# 退出码：有失败项 → 1（可用作发布前门槛）。**不可判定不算失败、也不算通过**，单列计数；
#        要判断「会不会是环境让本项测不到」，看小结里的不可判定条数。
#
# 副作用与恢复：脚本会临时改 VPP（建/删一个 BD、翻转某接口 admin 状态、在**开发态实例**里
# 建一个交换机并删除）。S10（配置编辑锁语义）另外会：① 用两个 REST 会话取锁并**提交一次
# 内容未变的修订**（造出「干净锁」现场，不改配置语义）；② 临时持有/释放编辑锁——每一项结束前
# 都释放，异常中断时下一遍开头的清理会兜底。结束时会尽力恢复；仍建议随后
# `systemctl restart vpp` + `restart nfvis` 以清掉残留拓扑（脚本会打印提醒）。
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
# S8 正控观察窗口：每 1s 采一次成员口 rx、共这么多次采样（见 s8_rx_window）。20s 足以跨过
# fixture guest beat 的超时慢周期（~3s/发）——单次 3s 窗口会整窗跨零（决策 #343 / R115-1）。
S8_WINDOW_SECS=${S8_WINDOW_SECS:-20}
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
# <tag> → 该 BD 的 **BD-ID**。⚠️ `vppctl show bridge-domain <参数>` 认的是 **BD-ID**（不是 Index），
# 而成员表查询 `vpp_bd_members` 直接吃 BD-ID。此前 S8 把 `vpp_bd_index_of_tag` 的结果（Index）喂给
# 成员表 ⇒ `show bridge-domain 1` 报 "No such bridge domain 1" ⇒ **成员表恒空**
# （round106 报告的「成员: （空）」的真因之一；同族缺陷已在 cli-lifecycle-check.sh 里修过并写明）。
vpp_bd_id_of_tag() {
  local tag="$1" id
  for id in $(vpp_bd_ids); do
    [ "$(vpp_bd_tag "$id")" = "$tag" ] && { echo "$id"; return; }
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
# <ifname> → 该口 rx 包数（无该口/取不到 → 0）。用于「这个窗口内有没有帧从成员口进来」。
# ⚠️ VPP 把「rx packets」打印在**接口名下那一行**（`<名> <idx> <state> <mtu> rx packets <n>`），
# 只有 rx bytes/drops/ip4 等才是缩进的续行——故不能按 `$1=="rx"` 认行（那样**恒得 0**；
# 2026-10-01 round107 实测：S8 报告「有 rx 计数的成员: （无）」的真因之一就是它）。
# 正确做法：在本口的输出里找相邻两列恰为 `rx packets` 的位置，取其后一列。
vpp_if_rx() {
  vppctl show interface "$1" 2>/dev/null | tr -d '\r' \
    | awk '{for (i = 1; i < NF; i++) if ($i == "rx" && $(i+1) == "packets") {print $(i+2); exit}}' \
    | grep -E '^[0-9]+$' || echo 0
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
# <BD-Idx> → 该 BD 上**非静态、非 BVI 自身**的 l2fib 学习条目「MAC 接口名」（MAC 小写）。
# verbose 表的列序固定（Mac-Address BD-Idx If-Idx BSN-ISN Age static filter bvi … Interface-Name）：
# $2=BD-Idx、$6=static 标志、$8=bvi 标志、$NF=Interface-Name。BVI 的 b0:b0:… 是接口自身的合成 MAC，
# 不是「学来的」，故按 $8 排除；static 条目同理（$6）。判 S8 时只认「成员口上真的学到了 MAC」。
vpp_l2fib_learned() {
  vppctl show l2fib verbose 2>/dev/null | tr -d '\r' \
    | awk -v bd="$1" 'NR > 1 && $2 == bd && $6 != "*" && $8 != "*" {print tolower($1), $NF}'
}
# <读视图文本> → 成员口展示名（逐行）。读视图 = `show virtual-switches <vs> ports`，决策 #326 起
# **与 REST GET /virtual-switches/{n}/ports 同源**：静态声明（source=config）+ VNF/容器声明的派生
# 条目（source=vnf|container）+ 仅在运行态存在的成员（source=runtime）。这是 S8 取成员的**唯一正源**
# （此前只看 `vppctl` 的 BD 成员表，看不见 link down 时不在 BD 里的派生端口）。
# 跳过表头（NR==1）与收尾的「（来源 vnf: …）」说明行；无成员时只有表头 + 一句说明，得空集属正常。
vsw_ports_parse() {
  printf '%s\n' "$1" | awk 'NR > 1 && $1 !~ /^（/ && NF >= 2 {print $1}'
}
# <vs-name> → 产品读视图里的成员口（调 CLI；解析在 vsw_ports_parse，便于自校准）。
vsw_readview_ports() { vsw_ports_parse "$(cli "show virtual-switches $1 ports")"; }
# <空格分隔的 rx 采样值> → 「增长周期数」（相邻采样增长记 1 次）。纯函数，供自校准抽取：
#   `10 12 12 15` → 2；全 0 → 0；单值 → 0；非数字按 0 容错。把「一瞬」的比对换成「一段时间」的
#   计数——fixture guest beat 在 ping 超时时降为 ~3s/发，单次 3s 窗口会整窗跨零（决策 #343 / R115-1）。
s8_rx_cycles_of() {
  local prev="" v n=0
  for v in $1; do
    case "$v" in ''|*[!0-9]*) v=0;; esac
    if [ -n "$prev" ] && [ "$v" -gt "$prev" ]; then n=$((n + 1)); fi
    prev="$v"
  done
  printf '%s' "$n"
}
# <成员口> [<采样次数>] → 逐口一行「<口> <增长周期数>」：**共用一个观察窗口**——每 1s 把各成员口
#   各采一次、共 N 次（默认 S8_WINDOW_SECS=20，故整窗约 N 秒、不随成员口数放大），每口的采样序列
#   交给 s8_rx_cycles_of 计周期。每口至少 2 个增长周期才算「确实有帧从成员口进来」——慢周期下也
#   不会整窗跨零（决策 #343 / R115-1）。
s8_rx_window() {
  local members="$1" secs="${2:-${S8_WINDOW_SECS:-20}}" m i=0 idx=0
  local -a acc=()
  for m in $members; do acc+=(""); done
  while [ "$i" -lt "$secs" ]; do
    idx=0
    for m in $members; do
      acc[idx]="${acc[idx]} $(vpp_if_rx "$m")"
      idx=$((idx + 1))
    done
    i=$((i + 1))
    [ "$i" -lt "$secs" ] && sleep 1
  done
  idx=0
  for m in $members; do
    printf '%s %s\n' "$m" "$(s8_rx_cycles_of "${acc[idx]}")"
    idx=$((idx + 1))
  done
}
# <空格分隔的成员口> → 这些口里 **VPP 真的认得** 的那些（读视图的派生条目在没落地时只有
# `<owner>/<nic>` 展示名，rx/邻居/学习都只在 VPP 真有的接口上才有意义）。
s8_members_in_vpp() {
  local out="" m
  for m in $1; do
    case " $out " in *" $m "*) continue;; esac
    vpp_ifaces | grep -qx -- "$m" && out="$out $m"
  done
  printf '%s' "${out# }"
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
# S8：<CLI 条数> <VPP l2fib 条数> <正控状态> <l2fib 里是否有成员口学到的 MAC>
#   正控状态：learned —— 观察窗口内确实有帧从**成员口**进来（本次 ping 的应答，或既有业务流量）
#             noflow  —— 有成员，但窗口内该交换机没有任何帧从成员口进来
#             nopeer  —— 有成员，但既取不到可 ping 的对端、也观察不到成员口流量
#             nomember—— 读视图与 VPP 都取不到该交换机的成员口
# 判据（**没有正向控制就没有通过**）：
#   缺正控 → UNKNOWN（不是 PASS——「两边都空」曾被算通过，于是没流量时永远绿）；
#   有正控 → 事实源必须真的有**成员口学到的 MAC**（非静态、非 BVI 自身；学不到即失败），
#            且与 CLI 条数一致（「有流量时两侧必须一致」正是本项的语义）。
s8_verdict() { # <cli_n> <fib_n> <control> <member_mac_learned>
  local cli_n="$1" fib_n="$2" ctl="$3" mac="$4"
  case "$ctl" in
    nopeer)   printf 'UNKNOWN:有成员但既无可 ping 对端也观察不到成员口流量（缺正向控制）'; return;;
    nomember) printf 'UNKNOWN:读视图与 VPP 都取不到该交换机的成员口（缺正向控制）'; return;;
    noflow)   printf 'UNKNOWN:有成员但窗口内无任何帧从成员口进来（缺正向控制）'; return;;
    learned)  ;;
    *)        printf 'UNKNOWN:正控状态未知（%s）' "$ctl"; return;;
  esac
  if [ "${fib_n:-0}" -eq 0 ]; then
    printf 'FAIL:成员口有流量但事实源没学到任何表项（l2fib 0 条，CLI %s 条）' "$cli_n"; return
  fi
  if [ "$mac" != "yes" ]; then
    printf 'FAIL:成员口有流量、事实源有 %s 条，但没有一条是成员口学到的 MAC（学到的不是本次流量的来源）' "$fib_n"; return
  fi
  if [ "$cli_n" -eq "$fib_n" ]; then
    printf 'PASS:两侧一致（CLI %s 条 / VPP %s 条，成员口已学到 MAC）' "$cli_n" "$fib_n"
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
# 成员枚举（决策 #328）：与**产品读视图同源** —— `show virtual-switches <vs> ports`（决策 #326 起
# 已含 VNF/容器声明的派生条目 source=vnf|container 与运行态成员 source=runtime），再用 `vppctl` 的
# BD 成员表**双向印证**：VPP 有而读视图没有 ⇒ 读视图漏成员，如实报（不静默取其一——r106 就是
# 前者看不见 link down 的派生端口，后者看不见未落地的声明）。
# 正控：先按邻居表找可 ping 的对端（源取同 BD 的 L3 口/BVI），再看**成员口 rx 在观察窗口内是否真的增长**
# （决策 #343：每 1s 采一次、共 S8_WINDOW_SECS 次，按「相邻采样增长即记 1 周期」计数，任一成员口 ≥2 周期
# 即判有流量——单次 3s 窗口会撞上 fixture guest beat 的超时慢周期 ~3s/发而整窗跨零，R115-1）；
# 取不到对端时仅看成员口 rx 是否增长（既有业务流量穿过该交换机同样是正控）。
# 两者都无 ⇒ 如实标不可判定——**没有把「这一轮没测到」变成「通过」**。
# 现场没有在交换机里收发帧的对象时，用 contrib/scripts/cli-semantic-fixture.sh up 造现场（见其头部）。
s8_tag=""; s8_idx=""; s8_member=""; s8_control=nomember; s8_mac=no; s8_memvpp=""
for t in $(cli_bdnames); do
  case "$t" in -|"") continue;; esac
  i=$(vpp_bd_index_of_tag "$t"); [ -n "$i" ] || continue    # Index：l2fib 的 BD-Idx 列用它
  bid=$(vpp_bd_id_of_tag "$t")                             # BD-ID：成员表查询用它（两者不同！）
  rv=$(vsw_readview_ports "$t" | tr '\n' ' '); rv=${rv% }
  vp=$(vpp_bd_members "$bid" | tr '\n' ' '); vp=${vp% }
  miss=""
  for m in $vp; do case " $rv " in *" $m "*) ;; *) miss="$miss $m";; esac; done
  echo "    交换机 $t（bd-id=$bid index=$i）"
  echo "      产品读视图成员: [${rv:-（无）}]"
  echo "      VPP BD 成员:    [${vp:-（无）}]${miss:+   ← **VPP 有而读视图缺:$miss**}"
  if [ -n "$miss" ]; then
    bad "S8 成员枚举：$t 的读视图缺少 VPP 中的成员：$miss（读视图应含运行态成员）"
  fi
  memvpp=$(s8_members_in_vpp "$rv $vp")
  if [ -z "$memvpp" ]; then
    echo "      （读视图/VPP 的成员里没有 VPP 认得的接口，跳过）"
    continue
  fi
  [ -z "$s8_memvpp" ] && { s8_tag="$t"; s8_idx="$i"; s8_memvpp="$memvpp"; s8_control=nopeer; }
  # 流量源：本 BD 里带 L3 地址的口（BVI/网关）；取不到就不指定源。
  src=""
  for m in $memvpp; do [ -n "$(vpp_if_l3 "$m")" ] && { src="$m"; break; }; done
  # 对端：邻居表里接口属于本 BD 成员的地址（在 BVI 上的邻居也确在本 BD 内）。
  peer=""
  while read -r pip pmac pif; do
    [ -n "$pif" ] || continue
    case " $memvpp " in *" $pif "*) peer="$pip"; break;; esac
  done <<EOF
$(vpp_neighbors)
EOF
  if [ -n "$peer" ]; then
    if [ -n "$src" ]; then
      pingout=$(vppctl ping "$peer" source "$src" repeat 5 2>&1 | tr -d '\r')
    else
      pingout=$(vppctl ping "$peer" repeat 5 2>&1 | tr -d '\r')
    fi
    sent=$(printf '%s' "$pingout" | sed -n 's/.*Statistics: *\([0-9][0-9]*\) sent.*/\1/p')
    echo "      制造流量: ping $peer${src:+ source $src} → $(printf '%s' "$pingout" | grep -E 'Statistics:' | head -1)"
    s8_control=noflow    # 有对端但还没证明帧从成员口进来（下面按 rx 增长定论）
  fi
  # 正控取样（决策 #343）：窗口内「增长周期计数」——每 1s 采一次成员口 rx、共 S8_WINDOW_SECS 次；
  # 任一成员口 ≥2 个增长周期即判 learned（单次 3s 窗口在 beat 超时慢周期下会整窗跨零）。
  win=$(s8_rx_window "$memvpp" "$S8_WINDOW_SECS")
  grew=$(printf '%s\n' "$win" | awk 'NF >= 2 && $2 + 0 >= 2 {print $1}' | tr '\n' ' '); grew=${grew% }
  winline=$(printf '%s\n' "$win" | awk 'NF >= 2 {printf "%s%s=%s 周期", (n++ ? "  " : ""), $1, $2}')
  echo "      成员口 rx ${S8_WINDOW_SECS}s 窗口: ${winline:-（无采样）}"
  if [ -n "$grew" ]; then
    s8_tag="$t"; s8_idx="$i"; s8_memvpp="$memvpp"; s8_member="$grew"; s8_control=learned
    break
  fi
done
echo "    候选交换机: ${s8_tag:-（无）} ｜ VPP 成员: ${s8_idx:+$(vpp_bd_members "$s8_idx" | tr '\n' ' ')}｜ 有流量的成员: ${s8_member:-（无）}"
if [ -z "$s8_idx" ]; then
  s8_cli_n=0; s8_fib_n=0
else
  out=$(cli "show virtual-switches $s8_tag mac-table")
  if echo "$out" | grep -q 'MAC 表为空'; then s8_cli_n=0
  else s8_cli_n=$(echo "$out" | awk 'NR>1 && $1 ~ /:/ {n++} END {print n+0}'); fi
  s8_fib_n=$(vpp_l2fib_count "$s8_idx")
  echo "    CLI mac-table 条数=$s8_cli_n；VPP l2fib 条数=$s8_fib_n（$s8_tag index=${s8_idx:-未在 VPP 中找到}）"
  if [ "$s8_control" = learned ]; then
    learned=$(vpp_l2fib_learned "$s8_idx")
    echo "    l2fib 非静态/非 BVI 学习条目: $(printf '%s' "$learned" | tr '\n' '; ')"
    while read -r _mac ifc; do
      [ -n "$ifc" ] || continue
      case " $s8_memvpp " in *" $ifc "*) s8_mac=yes; break;; esac
    done <<EOF
$learned
EOF
    echo "    成员口是否学到 MAC: $s8_mac（成员口集合: $s8_memvpp）"
    out=$(cli "show virtual-switches $s8_tag mac-table")
    if echo "$out" | grep -q 'MAC 表为空'; then s8_cli_n=0
    else s8_cli_n=$(echo "$out" | awk 'NR>1 && $1 ~ /:/ {n++} END {print n+0}'); fi
    s8_fib_n=$(vpp_l2fib_count "$s8_idx")     # 复测事实源（学习发生在本次流量之后）
    echo "    正控后 CLI mac-table 条数=$s8_cli_n；VPP l2fib 条数=$s8_fib_n"
  fi
fi
judge "S8 MAC 表 ↔ l2fib" "$(s8_verdict "${s8_cli_n:-0}" "${s8_fib_n:-0}" "$s8_control" "$s8_mac")"
if [ "$s8_control" != learned ]; then
  echo "    · 造正控现场：bash contrib/scripts/cli-semantic-fixture.sh up（在交换机里起一台持续收发的 VNF；见该脚本头部）"
fi

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

# ============ S10 配置编辑锁语义（决策 #317/#318）============
# 为什么在这里、为什么用 REST：这两条决策的判据是**两个会话之间**的排他与接管——单条 CLI
# 命令是一次性会话（收尾即释放），表达不了；只有 REST 能同时持两个 token。三个事实源
# （HTTP 状态码 + 响应体 + 会话视图）都要对上，且**每项先立正向控制**（现场真的处于那个状态），
# 再判结果——否则「什么都没发生」会被算成通过（round84 之后本脚本的既定口径）。
# 副作用：两个会话都只是「取锁 + 空 merge（内容与 committed 相同）」——后者提交一次**内容未变**的
# 修订，不改变机器的配置语义；结束前释放锁，不留现场。
hdr "S10 配置编辑锁语义：同一用户两会话的排他 / 接管 / 释放"
S10B=/tmp/cli-s10-body-$$.txt
login_json() { curl_api -X POST "$SRV/api/v1/login" -H 'Content-Type: application/json' \
                 -d "{\"username\":\"admin\",\"password\":\"$PW\"}"; }
jfield() { sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p" | head -1; }
cand_put() { # cand_put <token>：空 merge（override 后候选=committed，再 merge {} 不改内容）
  curl_api -o "$S10B" -w '%{http_code}' -X PUT "$SRV/api/v1/configuration/candidate" \
    -H "Authorization: Bearer $1" -H 'Content-Type: application/json' -H 'X-NFVIS-Merge: true' -d '{}'
}
cand_del()   { curl_api -o /dev/null -w '%{http_code}' -X DELETE "$SRV/api/v1/configuration/candidate" -H "Authorization: Bearer $1"; }
cand_dirty() { curl_api "$SRV/api/v1/configuration/candidate" -H "Authorization: Bearer $1" \
                 | sed -nE 's/.*"dirty":(true|false).*/\1/p' | head -1; }
commit_now() { curl_api -o "$S10B" -w '%{http_code}' -X POST "$SRV/api/v1/configuration/commit" \
                 -H "Authorization: Bearer $1" -H 'Content-Type: application/json' -d '{}'; }
logout_now() { curl_api -o /dev/null -w '%{http_code}' -X POST "$SRV/api/v1/logout" -H "Authorization: Bearer $1"; }

A_JSON=$(login_json); A_TOK=$(printf '%s' "$A_JSON" | jfield token); A_ID=$(printf '%s' "$A_JSON" | jfield token_id)
B_JSON=$(login_json); B_TOK=$(printf '%s' "$B_JSON" | jfield token); B_ID=$(printf '%s' "$B_JSON" | jfield token_id)
if [ -z "$A_TOK" ] || [ -z "$B_TOK" ] || [ -z "$A_ID" ] || [ -z "$B_ID" ]; then
  bad "S10 前置：两次登录未取到 token/token_id（无法做两会话对照）"
else
  cand_del "$A_TOK" >/dev/null; cand_del "$B_TOK" >/dev/null   # 清上一轮异常中断可能留下的锁

  # S10-1 脏候选严格排他（#318 边界：dirty=true 绝不丢弃他人未提交改动）
  rc=$(cand_put "$A_TOK"); d=$(cand_dirty "$A_TOK")
  if [ "$rc" = "200" ] && [ "$d" = "true" ]; then
    ok "S10-1 正控：A 写入候选成功且 dirty=true（$rc/$d）"
    rc=$(cand_put "$B_TOK"); body=$(cat "$S10B" 2>/dev/null)
    if [ "$rc" = "409" ] && printf '%s' "$body" | grep -q '持有'; then
      ok "S10-1 脏候选严格排他：同用户另一会话写入被拒（409，说明持有者）"
    else
      bad "S10-1 期望 409 且说明持有者，实际 $rc：$(printf '%s' "$body" | head -c 200)"
    fi
  else
    bad "S10-1 正控不成立（A 写入 $rc，dirty=$d）——本项不继续判定"
  fi

  # S10-2 干净锁不排他（#318 口径①）——先造出「A 持锁但候选干净」的现场
  rc=$(commit_now "$A_TOK"); d=$(cand_dirty "$A_TOK")
  if [ "$rc" = "200" ] && [ "$d" = "false" ]; then
    ok "S10-2 正控：A 提交（内容未变）后仍持锁、候选干净（dirty=false）"
    rc=$(cand_put "$B_TOK"); body=$(cat "$S10B" 2>/dev/null)
    if [ "$rc" = "200" ]; then
      ok "S10-2 干净锁不排他：同用户新会话接管成功（200）"
    else
      bad "S10-2 干净锁接管失败（实际 $rc）：$(printf '%s' "$body" | head -c 200)"
    fi
  else
    bad "S10-2 正控不成立：A 提交返回 $rc、dirty=$d（$(head -c 200 "$S10B" 2>/dev/null)）"
  fi

  # S10-3 被接管者得明确错误（#318：ErrLockLost，不静默把现场接管回自己）
  rc=$(cand_put "$A_TOK"); body=$(cat "$S10B" 2>/dev/null)
  if [ "$rc" = "409" ] && printf '%s' "$body" | grep -q '接管'; then
    ok "S10-3 被接管者得明确错误（409，文案含「接管」）"
  elif [ "$rc" = "409" ]; then
    bad "S10-3 被接管者确被拒（409）但文案未说明被接管：$(printf '%s' "$body" | head -c 200)"
  else
    bad "S10-3 被接管者竟仍能写入（$rc）——ErrLockLost 未生效"
  fi

  # S10-4 被接管者登出不影响接管者的脏候选（#317 的保护语义不被 #318 的释放路径误伤）
  d=$(cand_dirty "$B_TOK")
  if [ "$d" = "true" ]; then ok "S10-4 正控：接管者 B 现持脏候选（dirty=true）"
  else bad "S10-4 正控不成立：B 的候选 dirty=$d"; fi
  rc=$(logout_now "$A_TOK"); d2=$(cand_dirty "$B_TOK")
  if [ "$rc" = "204" ] && [ "$d2" = "true" ]; then
    ok "S10-4 被接管者登出后接管者的脏候选原封不动（仍 200/dirty=true）"
  else
    bad "S10-4 登出（$rc）后接管者候选被动到（dirty=$d2）——同一用户的其它会话被误清"
  fi

  # S10-5 会话视图如实列会话标识与用户（#317③）
  sessions=$(cli "show configuration sessions")
  if printf '%s' "$sessions" | grep -q 'Session' && printf '%s' "$sessions" | grep -q 'User' \
     && printf '%s' "$sessions" | grep -qF "$B_ID"; then
    ok "S10-5 会话视图列 Session/User 且含持锁会话标识（$B_ID 与所属用户一并可见）"
  else
    bad "S10-5 会话视图缺列或缺持锁会话标识：$(printf '%s' "$sessions" | head -3 | tr '\n' ' ')"
  fi

  # S10-6 接管者释放后原用户可重新进入（#318：superseded 记录随释放失效）
  rc=$(cand_del "$B_TOK")
  if [ "$rc" = "204" ]; then ok "S10-6 正控：接管者释放锁（204）"
  else bad "S10-6 正控不成立：接管者释放锁返回 $rc"; fi
  A2_JSON=$(login_json); A2_TOK=$(printf '%s' "$A2_JSON" | jfield token)
  if [ -z "$A2_TOK" ]; then
    bad "S10-6 前置：重新登录失败（无法验证释放后可重新进入）"
  else
    rc=$(cand_put "$A2_TOK")
    if [ "$rc" = "200" ]; then ok "S10-6 接管者释放后原用户的新会话可重新进入（200）"
    else bad "S10-6 重新进入失败（$rc）：$(head -c 200 "$S10B" 2>/dev/null)"; fi

    # S10-7 登出按会话清理本会话锁（#318 口径②）——A2 的候选是**脏**的，
    # 若登出没释放锁，B 的写入必然 409：这是一个能区分「真的释放了」的判据。
    rc=$(logout_now "$A2_TOK"); rc2=$(cand_put "$B_TOK")
    if [ "$rc" = "204" ] && [ "$rc2" = "200" ]; then
      ok "S10-7 登出按会话清理本会话锁（A2 登出后 B 可立即取锁）"
    else
      bad "S10-7 登出未释放本会话锁（logout=$rc，随后写候选=$rc2）：$(head -c 200 "$S10B" 2>/dev/null)"
    fi
    cand_del "$B_TOK" >/dev/null   # 收尾：不留锁

    # S10-8 逐 token 吊销正向路径（#301）：super-user 吊销另一会话 → 其下一个请求 401
    C_JSON=$(login_json); C_TOK=$(printf '%s' "$C_JSON" | jfield token); C_ID=$(printf '%s' "$C_JSON" | jfield token_id)
    if [ -z "$C_TOK" ] || [ -z "$C_ID" ]; then
      unk "S10-8 逐 token 吊销 —— 第三个登录未取到 token/token_id"
    else
      rev=$(curl_api -o "$S10B" -w '%{http_code}' -X POST "$SRV/api/v1/system/api-tokens/$C_ID:revoke" \
              -H "Authorization: Bearer $B_TOK")
      after=$(curl_api -o /dev/null -w '%{http_code}' "$SRV/api/v1/system/version" -H "Authorization: Bearer $C_TOK")
      if [ "$rev" = "204" ] && [ "$after" = "401" ]; then
        ok "S10-8 逐 token 吊销：被吊销会话的下一个请求 401（$rev → $after）"
      elif [ "$rev" != "204" ]; then
        bad "S10-8 吊销未成功（$rev）：$(head -c 200 "$S10B" 2>/dev/null)"
      else
        bad "S10-8 吊销返回 204 但被吊销 token 仍可用（$after）——吊销未真正生效"
      fi
    fi
  fi
  logout_now "$B_TOK" >/dev/null
  rm -f "$S10B"
fi

# ---------- S11 DHCP 中继（决策 #335）：产品读视图 ↔ vppctl show dhcp proxy ----------
# 有 relay 配置才可判定：现场没有 relay 时缺正向控制，如实报「不可判定」、**不计入通过**
# （round84 三档口径）。oracle：`vppctl show dhcp proxy` 的行须同时含中继源地址（BVI 的
# IPv4 网关）与 server 地址（该行的 RX FIB 表就是网关转发域）；CRLF 行尾先剥（本脚本已知坑）。
hdr "S11 DHCP 中继：产品读视图 ↔ vppctl show dhcp proxy"
relay_json=$(curl_api "$SRV/api/v1/configuration" -H "Authorization: Bearer $TOKEN" 2>/dev/null)
if [ -z "$relay_json" ]; then
  unk "S11 取不到产品配置（GET /configuration 失败），无法对照"
elif ! command -v python3 >/dev/null 2>&1; then
  unk "S11 无 python3（解析配置 JSON 用），无法对照——如实登记"
else
  relay_rows=$(printf '%s' "$relay_json" | python3 -c '
import json, sys
try:
    cfg = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for vs in (cfg.get("virtual_switches") or []):
    srv = vs.get("dhcp_relay_server")
    if not srv:
        continue
    src = ""
    for a in ((vs.get("gateway") or {}).get("addresses") or []):
        ip = a.split("/")[0]
        if ":" not in ip:
            src = ip
            break
    print("%s	%s	%s" % (vs.get("name", ""), srv, src))
')
  if [ -z "$relay_rows" ]; then
    unk "S11 现场没有 relay 配置（无对象可对照）——造现场：set virtual-switches <vs> gateway ip … + dhcp-relay server … 后复跑"
  else
    vpp_proxy=$(vppctl show dhcp proxy 2>/dev/null | tr -d '
')
    echo "    vppctl show dhcp proxy:"; printf '%s
' "$vpp_proxy" | sed 's/^/      | /' | head -5
    if [ -z "$vpp_proxy" ]; then
      bad "S11 vppctl show dhcp proxy 无输出（oracle 取不到事实）"
    fi
    while IFS="$(printf '	')" read -r vsn srv src; do
      [ -n "$vsn" ] || continue
      echo "    对象: $vsn  server=$srv  src=$src"
      rv=$(curl_api "$SRV/api/v1/virtual-switches/$vsn" -H "Authorization: Bearer $TOKEN" 2>/dev/null)
      if printf '%s' "$rv" | grep -q '"dhcp_relay" *"server" *: *"[^"]*'$srv'"'; then
        ok "S11 $vsn 产品读视图带 dhcp_relay.server=$srv"
      else
        bad "S11 $vsn 产品读视图缺 dhcp_relay.server（或值非 $srv）：$(printf '%s' "$rv" | head -c 160)"
      fi
      if printf '%s
' "$vpp_proxy" | grep -q "$src" &&
         printf '%s
' "$vpp_proxy" | grep "$src" | grep -q "$srv"; then
        ok "S11 $vsn 的 relay 与 VPP 一致（src=$src server=$srv 同行）"
      elif printf '%s
' "$vpp_proxy" | grep -q "$src"; then
        bad "S11 $vsn 在 show dhcp proxy 里只有 src 无 server=$srv：$(printf '%s
' "$vpp_proxy" | grep "$src" | head -2)"
      else
        bad "S11 show dhcp proxy 里找不到 $vsn 的中继源 $src（relay 未下发或未随恢复重放）"
      fi
    done <<RELAY_ROWS
$relay_rows
RELAY_ROWS
  fi
fi

# ---------- S12 MAC 学习上限（决策 #337）：产品读视图 learn_limit ↔ vppctl Learn-li ----------
# 有 learn-limit 配置才可判定：现场没有该配置时缺对照对象，如实报「不可判定」、**不计入通过**
# （与 S11 同口径）。oracle：`vppctl show bridge-domain <bd-id> detail` 里带 Learn-li/limit 的那一行
# 的整数（CRLF 行尾先剥）。⚠️ binapi v0.13.0 的 bridge_domain_details 没有 learn_limit 字段，
# 故这条 vppctl oracle 是「产品真下发到数据面」的**唯一独立事实源**。
vpp_bd_learn_limit() { # <BD-ID> → vppctl 报告的 MAC 学习上限（取不到回空 ⇒ 判不可判定）
  vppctl show bridge-domain "$1" detail 2>/dev/null | tr -d '\r' \
    | awk 'tolower($0) ~ /learn.?li/ {for (i = 1; i <= NF; i++) if ($i ~ /^[0-9]+$/) {print $i; exit}}'
}
hdr "S12 MAC 学习上限：产品读视图 learn_limit ↔ vppctl show bridge-domain detail 的 Learn-li"
ll_cfg=$(curl_api "$SRV/api/v1/configuration" -H "Authorization: Bearer $TOKEN" 2>/dev/null)
if [ -z "$ll_cfg" ]; then
  unk "S12 取不到产品配置（GET /configuration 失败），无法对照"
elif ! command -v python3 >/dev/null 2>&1; then
  unk "S12 无 python3（解析配置 JSON 用），无法对照——如实登记"
else
  ll_rows=$(printf '%s' "$ll_cfg" | python3 -c '
import json, sys
try:
    cfg = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for vs in (cfg.get("virtual_switches") or []):
    if vs.get("type") != "l2":
        continue
    n = vs.get("learn_limit")
    if not n:
        continue
    print("%s\t%d" % (vs.get("name", ""), n))
')
  if [ -z "$ll_rows" ]; then
    unk "S12 现场没有 learn-limit 配置（无对象可对照）——造现场：set virtual-switches <vs> learn-limit <n> 后复跑"
  else
    while IFS="$(printf '\t')" read -r vsn want; do
      [ -n "$vsn" ] || continue
      bid=$(vpp_bd_id_of_tag "$vsn")
      echo "    对象: $vsn  声明 learn-limit=$want  bd-id=${bid:-（vppctl 里找不到）}"
      rv=$(curl_api "$SRV/api/v1/virtual-switches/$vsn" -H "Authorization: Bearer $TOKEN" 2>/dev/null)
      if printf '%s' "$rv" | grep -q '"learn_limit" *: *'$want; then
        ok "S12 $vsn 产品读视图带 learn_limit=$want"
      else
        bad "S12 $vsn 产品读视图缺 learn_limit=$want（或值不同）：$(printf '%s' "$rv" | head -c 160)"
      fi
      if [ -z "$bid" ]; then
        bad "S12 $vsn 在 vppctl 里找不到该交换机的 bridge-domain（未落地？）"
        continue
      fi
      got=$(vpp_bd_learn_limit "$bid")
      if [ -z "$got" ]; then
        unk "S12 $vsn oracle 取不到 Learn-li（bd-id=$bid）——vppctl detail 无该字段，本项不可判定"
      elif [ "$got" = "$want" ]; then
        ok "S12 $vsn VPP Learn-li=$got 与产品声明一致（bd-id=$bid）"
      else
        bad "S12 $vsn VPP Learn-li=$got ≠ 产品声明 $want（bd-id=$bid）——下发未生效或未随恢复重放"
      fi
    done <<LL_ROWS
$ll_rows
LL_ROWS
  fi
fi

# ---------- S13 数据面 DNS 代理（决策 #345）：产品读视图 ↔ CLI ↔ VPP punt 注册 ----------
# 有 DNS 代理配置才可判定正控：现场没配（enabled=false）或缺 VPP 侧 oracle 时如实报「不可判定」，
# **不计入通过**（与 S11/S12 三档口径一致）。oracle：VPP punt socket 注册（`vppctl show punt socket
# registrations l4`，round124 真机实测该形态可用；裸 `show punt socket` 会报 unknown input）
# 里应有 nfvisd 的 client socket（/run/vpp/nfvis-dns.sock）——即「产品真的注册了 punt」的独立事实源；
# 该命令在底座不存在或无输出时如实报不可判定（不伪造）。
hdr "S13 数据面 DNS 代理：产品读视图 ↔ CLI show dns proxy ↔ VPP punt 注册"
dnsview=$(curl_api "$SRV/api/v1/dns/proxy" -H "Authorization: Bearer $TOKEN" 2>/dev/null)
if [ -z "$dnsview" ]; then
  unk "S13 取不到产品读视图（GET /dns/proxy 失败），无法对照"
elif ! command -v python3 >/dev/null 2>&1; then
  unk "S13 无 python3（解析读视图 JSON 用），无法判定——如实登记"
else
  dns_enabled=$(printf '%s' "$dnsview" | python3 -c 'import json,sys
try: v=json.load(sys.stdin)
except Exception: print(""); sys.exit(0)
print("true" if v.get("enabled") else "false")')
  cli_view=$(cli "show dns proxy" 2>/dev/null)
  if printf '%s' "$cli_view" | grep -q '%%'; then
    bad "S13 CLI show dns proxy 报错：$(printf '%s' "$cli_view" | head -c 160)"
  elif [ "$dns_enabled" = "true" ] && ! printf '%s' "$cli_view" | grep -q '启用'; then
    bad "S13 REST enabled=true 但 CLI 显示未启用（两面不同源）"
  elif [ "$dns_enabled" = "false" ] && ! printf '%s' "$cli_view" | grep -q '未配置'; then
    bad "S13 REST enabled=false 但 CLI 未显示未配置：$(printf '%s' "$cli_view" | head -c 160)"
  else
    ok "S13 CLI 与 REST 读视图一致（enabled=$dns_enabled）"
  fi

  if [ "$dns_enabled" = "true" ]; then
    if ! command -v vppctl >/dev/null 2>&1; then
      unk "S13 无 vppctl，无法核对 VPP punt 注册——如实登记"
    else
      punt=$(vppctl show punt socket registrations l4 2>/dev/null | tr -d '\r')
      echo "    vppctl show punt socket registrations l4:"; printf '%s\n' "$punt" | sed 's/^/      | /' | head -6
      if [ -z "$punt" ]; then
        unk "S13 vppctl show punt socket registrations l4 无输出（oracle 取不到事实；底座可能无该命令）——如实报不可判定"
      elif printf '%s\n' "$punt" | grep -q 'nfvis-dns.sock'; then
        ok "S13 VPP 侧存在 punt 注册（nfvisd client socket 在场，与 enabled=true 一致）"
      else
        bad "S13 enabled=true 但 VPP 侧无 nfvisd 的 punt 注册——命令成功而数据面未生效（假功能）"
      fi
    fi
  else
    unk "S13 现场没有 DNS 代理配置（enabled=false，无正向控制）——造现场：set system dns proxy server <ip> 提交后复跑"
  fi
fi

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
