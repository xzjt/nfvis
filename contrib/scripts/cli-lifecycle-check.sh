#!/usr/bin/env bash
# NFViS CLI 生命周期与组合检查（**手动**，真机跑，不进 CI / make check）
#
# 用途：量「套件从结构上看不见」的那一维——**生命周期与跨对象组合**。
#   既有的两套真机套件都在**第一次**动作上结束：cli-fulltest.sh 问「命令能不能用」，
#   cli-semantic-check.sh 问「结果对不对」，两者的对象都是**刚建好的**。
#   于是「建 → 用 → 删 → 回读」「重启后重放」「两个对象交叉组合」这三类缺陷全线漏网：
#   2026-09-26 的干净装机走查（round84）一次抓出 7 条同族缺陷，全在删除路径与重启路径上——
#   删 L3 交换机不清同名 VRF、删 bond 后 VPP 里 BondEthernet 残留、VPP 重启后统计永久不可用、
#   进程内状态不随 VPP 失效导致跳过重放、VNF 的 vNIC 从未挂进交换机、重启顺序造成的未收敛项。
#   它们的共同点：**命令全绿、零报错**，只有「删掉再读」「重启再读」「拿两个对象对账」才看得见。
#
# 本脚本的三段（每段都有**独立事实源**与**正向控制**，缺前置时明确「跳过 + 原因」并单列计数，
# 绝不静默通过）。执行顺序是 L1 → L3 → L2：重启会清空 ARP 与会话表，而组合那一段要靠它们
# 才可能拿到正向控制，故重启放最后（跑完机器也停在收敛态）：
#   L1 删对象后回读：建 L3 交换机（含 L3 接口地址与静态路由）+ bond + 端口镜像会话
#                    → 逐个删除 → 断言配置与建之前逐字段一致、运行态无残留、BD 成员还原。
#                    判据细节：接口仍绑着表时 VPP 对删表**返回成功却不删**（该表继续留在
#                    `show ip table` 里带 locks:[interface:…]，直到 VPP 重启），故「表还在」
#                    不再是登记项而是**断言**：删 L3 交换机 + commit 后该表（v4/v6 两张）必须从
#                    `show ip table` / `show ip6 table` 消失、该口必须回到默认表（地址行不带
#                    非默认 table-id）。
#                    本对象声明的静态路由仍留在表里同样是失败项（产品可控的撤销项）。
#                    端口镜像那项需要两个自由口（源口 + 分析口），本机只有一个时如实跳过。
#   L3 组合能力：VNF 的 vNIC 是否真的接进它声明的交换机（L2 看 BD 成员、L3 看它属于哪张表），
#                    并用 ping 该转发域里的主机（对端取自邻居表）作正向控制；NAT 会话与 VPP 会话表对账。
#   L2 重启/重放一致性：systemctl restart vpp（**不**重启 nfvisd）→ 等守护进程重连收敛
#                    → 断言统计仍可读、NAT 的 inside/outside 仍下在数据面、日志无未收敛项。
#
# ⚠️ L2 会**短暂中断数据面**（VPP 重启期间业务口不通，guest 可能掉线再恢复），结束时会如实报告终态。
# ⚠️ 本脚本会在配置里建对象，跑完自行删除；对象名统一带 rc84t- 前缀，便于识别与收尾。
#    它刻意**不碰**既有基线的对象（不依赖、不修改别人的交换机/VNF），只使用「能自由分配的口」。
#
# 前置（均在 nfvis-vm 上）：
#   1) nfvisd 已起；CLI_BIN 指向**同版本** CLI（已装实例用 /usr/bin/nfvis-cli）
#   2) VPP 运行中且 vppctl 可用（作为独立事实源）；L2 段需要 systemctl 能管 vpp
#   3) 口令经 NFVIS_PASSWORD 给（缺省是开发态口令）
#
# 用法：
#   SRV=https://127.0.0.1:443 CLI_BIN=/usr/bin/nfvis-cli NFVIS_PASSWORD='…' bash contrib/scripts/cli-lifecycle-check.sh
# 退出码：有失败项 → 1。**跳过（不可判定）不算失败、也不算通过**，单列计数。
set -u

SRV=${SRV:-http://127.0.0.1:18443}
CLI_BIN=${CLI_BIN:-/tmp/nfvis-cli}
PW=${NFVIS_PASSWORD:-Admin@12345}
PFX=${PFX:-rc84t}                     # 本脚本创建对象的前缀（收尾靠它识别）
VPP_UNIT=${VPP_UNIT:-vpp}             # VPP 的 systemd 单元名
NFVIS_UNIT=${NFVIS_UNIT:-nfvis}       # nfvisd 的 systemd 单元名
CONV_TIMEOUT=${CONV_TIMEOUT:-60}      # 等重连收敛的上限（秒）
V3="$PFX-l3"; B3="$PFX-b0"; S3="$PFX-span"          # 会被建/删的三个对象
ADDR3=10.99.88.1/24; ROUTE3=10.99.89.0/24; NH3=10.99.89.2

cli() { "$CLI_BIN" -server "$SRV" -u admin -p "$PW" -source console -c "$1" 2>&1 | grep -v '^连接'; }
cli_err() { printf '%s\n' "$1" | grep -qE '^%%|^%[^%]|^%$|^校验失败'; }

PASS=0; FAIL=0; SKIP=0; INFO=0
ok()   { echo "  ✓ $1"; PASS=$((PASS+1)); }
bad()  { echo "  ✗ $1"; FAIL=$((FAIL+1)); }
skip() { echo "  ⊘ 跳过：$1"; SKIP=$((SKIP+1)); }
note() { echo "  · $1"; INFO=$((INFO+1)); }
hdr()  { echo; echo "===== $1"; }
# 失败时的**最小对照**：期望 vs 实际（照既有套件的风格，只给判据本身，不铺全量输出）
exp_act() { echo "    期望: ${1:-（空）}"; echo "    实际: ${2:-（空）}"; }

# ---------- 独立事实源（oracle；与 cli-semantic-check.sh 的同名函数保持一致口径）----------
vpp_ifaces()      { vppctl show interface 2>/dev/null | tr -d '\r' | awk '/^[^ \t]/ && $2 ~ /^[0-9]+$/ {print $1}' | grep -v '^local0$' | sort; }
# 产品侧已知的 bond 名（`show bonds` 首列）。
#
# 为什么不能只按 `^BondEthernet` 前缀排除：**产品会给 bond 起自己的名字**（如 `bond0`），
# `vppctl show interface` 里显示的就是这个名字，而 `show bond details` 里才是 `BondEthernet0`。
# round86 实测（工具假红）：自由口枚举把 `bond0` 当成了「可自由分配的物理口」，于是 L1-2
# 建 bond 时拿另一个 bond 当成员口 → 被产品校验拒（「成员口 "bond0" 不是物理口」）→
# 看上去像「产品建不起 bond」。工具选错对象造成的假红与产品缺陷同样要修。
# `show bonds` 是配置视图（`name <bond>;` 行才是名字，首列是字段名），故按 name 行取。
product_bonds()   { "$CLI_BIN" -server "$SRV" -u admin -p "$PW" -source console -c "show bonds" 2>/dev/null | tr -d '\r' | sed -n 's/^[[:space:]]*name[[:space:]]\+\([^;]*\);.*/\1/p' | sort -u; }
vpp_phys_ports()  {
  local skip p
  skip=" $(product_bonds | tr '\n' ' ') "
  for p in $(vpp_ifaces | grep -vE '^BondEthernet|^bvi|^vh-|^tap|^memif'); do
    case "$skip" in *" $p "*) continue;; esac
    printf '%s\n' "$p"
  done
}
vpp_bond_set()    { vppctl show bond details 2>/dev/null | tr -d '\r' | awk '/^BondEthernet/ {print $1}' | sort; }
vpp_bond_members(){ vppctl show bond details 2>/dev/null | tr -d '\r' | awk 'NF==1 && $1 !~ /^BondEthernet/ && $1 !~ /:$/ {print $1}' | sort -u; }
vpp_bd_ids()      { vppctl show bridge-domain 2>/dev/null | tr -d '\r' | awk 'NR>1 && $1 ~ /^[0-9]+$/ {print $1}' | sort; }
vpp_bd_tag()      { vppctl show bridge-domain "$1" detail 2>/dev/null | tr -d '\r' | sed -n 's/.*BD-Tag: //p'; }
# 按交换机名（BD-Tag）找 bridge-domain 的 **BD-ID**。
# 注意返回的是 BD-ID，不是 Index：`vppctl show bridge-domain <参数>` 认 BD-ID，而本函数的唯一
# 调用方就是成员表比对（vpp_bd_members 也直接吃 BD-ID）。早先这里返回 Index（$2），于是成员表
# 查询永远为空——**工具假红**：直读 `show bridge-domain <BD-ID> detail` 明明有 vh- 成员，脚本
# 却报「vNIC 从未挂进 BD」（round85 首次把带 vNIC 的 VNF 交给本脚本时才暴露：#171 之后首次
# 真正走到 L3-1 的 L2 分支）。
vpp_bd_id_of_tag() {
  local tag="$1" id
  for id in $(vpp_bd_ids); do
    [ "$(vpp_bd_tag "$id")" = "$tag" ] && { echo "$id"; return; }
  done
}
# 成员表：首列接口名、次列数字 If-idx；无成员时 VPP 整块不打印成员表（空集合是正常结果）
vpp_bd_members() {
  local out names n rest
  out=$(vppctl show bridge-domain "$1" detail 2>/dev/null)
  names=$(vpp_ifaces | tr '\n' ' ')
  printf '%s\n' "$out" | while read -r n rest; do
    case "$rest" in [0-9]*) case " $names " in *" $n "*) echo "$n";; esac;; esac
  done
}
vpp_if_rx()       { vppctl show interface "$1" 2>/dev/null | tr -d '\r' | awk '$1=="rx" && $2=="packets" {print $3; exit}' | grep -E '^[0-9]+$' || echo 0; }
vpp_if_rxbytes()  { vppctl show interface "$1" 2>/dev/null | tr -d '\r' | awk '$1=="rx" && $2=="bytes" {print $3; exit}' | grep -E '^[0-9]+$' || echo 0; }
vpp_if_l3() {  # <ifname> → 该口的 L3 地址行（无则空）
  vppctl show interface addr 2>/dev/null | tr -d '\r' | awk -v n="$1" '
    $0 !~ /^[ \t]/ { cur = ($1 == n); next }
    cur && /L3 / { print; exit }'
}
vpp_neighbors() {  # 邻居表 → 「IP MAC 接口名」
  # 行格式（VPP 26.06 实测）：Time IP Flags Ethernet Interface（Flags 动态=D、静态=S）。
  # 列锚定**从行尾**取（$NF=接口、$NF-1=MAC），Flags 列为空导致字段左移时也不丢行。
  vppctl show ip neighbors 2>/dev/null | tr -d '\r' \
    | awk '$(NF-1) ~ /^([0-9a-fA-F][0-9a-fA-F]:){5}[0-9a-fA-F][0-9a-fA-F]$/ {print $2, tolower($(NF-1)), $NF}'
}
# <table-id> <vhost-if> → 该**转发域**里的邻居行「IP 接口名」。
# 决策 #334：VPP 26.06 的 `show ip neighbors` **没有 vrf/表过滤参数**（唯一参数是
# **位置参数的接口名**；无参＝v4+v6 全表混排、行内不带表号）——「无参只给表 0」是
# 当年现场的误读。故「按表取」只能按行内接口的表归属筛：
#   · 对端行就是 vhost 自己 → 它本身就是域成员，直接认；
#   · 否则对端行的口带地址、且地址行上的 table-id 等于该域 → 认；
#   · table-id 参数为空＝表 0 口径：只认地址行**不带** table-id 的口（原行为等价）。
vpp_domain_neighbors() {
  local tid="$1" ifn="$2" ip mac ifc it
  vpp_neighbors | while read -r ip mac ifc; do
    [ -n "$ifc" ] || continue
    if [ "$ifc" = "$ifn" ]; then printf '%s %s\n' "$ip" "$ifc"; continue; fi
    it=$(vpp_if_l3 "$ifc" 2>/dev/null | sed -n 's/.*table-id *\([0-9][0-9]*\).*/\1/p')
    [ "$it" = "$tid" ] && printf '%s %s\n' "$ip" "$ifc"
  done
}
vpp_table_id_of() { vppctl show ip table 2>/dev/null | tr -d '\r' | awk -v n="$1" '$NF == n {gsub("table_id:", "", $2); print $2; exit}'; }
# v6 表要单独读（VPP 的 `show ip table` 只列 IPv4，`show ip6 table` 才是 v6 视图）：
# v4/v6 两张表都要真的删掉，只看 v4 会漏掉 v6 的残留。
vpp_table_id_of6() { vppctl show ip6 table 2>/dev/null | tr -d '\r' | awk -v n="$1" '$NF == n {gsub("table_id:", "", $2); print $2; exit}'; }
vpp_tables()      { vppctl show ip table 2>/dev/null | tr -d '\r' | awk '/table_id:/ {print $NF}' | sort; }
vpp_nat_ifaces()  { vppctl show nat44 ei interfaces 2>/dev/null | tr -d '\r' | awk 'NF >= 2 && $NF ~ /^(in|out)$/ {print $1, $NF}' | sort; }
vpp_nat_sessions(){ vppctl show nat44 ei sessions detail 2>/dev/null | tr -d '\r' | awk '/^-+ thread/ {for (i=1;i<=NF;i++) if ($i=="sessions") s+=$(i-1)} END {print s+0}'; }
mgmt_iface()      { ip route show default 2>/dev/null | awk '{for (i=1;i<=NF;i++) if ($i=="dev") print $(i+1)}' | head -1; }
# 能自由分配的数据面口：物理口 − 管理口（红线）− 已有 L3 地址的 − bond 成员 − 已在某个 BD 里的
free_ports() {
  local p m
  for p in $(vpp_phys_ports); do
    [ "$p" = "$(mgmt_iface)" ] && continue
    [ -n "$(vpp_if_l3 "$p")" ] && continue
    case " $(vpp_bond_members | tr '\n' ' ') " in *" $p "*) continue;; esac
    m=0
    for i in $(vpp_bd_ids); do vpp_bd_members "$i" | grep -qx "$p" && m=1; done
    [ "$m" = 1 ] && continue
    printf '%s\n' "$p"
  done
}

# ---------- 前置自检（缺一即明确退出，别让断言以空输出形式连片假红）----------
if [ ! -x "$CLI_BIN" ]; then
  echo "✗ 找不到可执行的 CLI: $CLI_BIN（已装实例请给 CLI_BIN=/usr/bin/nfvis-cli）"
  exit 1
fi
if ! command -v vppctl >/dev/null 2>&1; then
  echo "✗ 找不到 vppctl：本脚本需要 VPP 作为独立事实源"
  exit 1
fi
sc=$(cli "show version" | head -3)
if ! printf '%s' "$sc" | grep -q NFViS; then
  echo "✗ CLI 无法通过 $SRV 取得输出（确认守护进程已起、口令正确、CLI 与服务端版本匹配）："
  printf '%s\n' "$sc" | sed 's/^/      /'
  exit 1
fi

BASE=$(mktemp /tmp/cli-lifecycle-base-XXXXXX.txt)
cli "show configuration" > "$BASE"
NFVIS_PASS_BEFORE=$(vpp_nat_ifaces | tr '\n' ' ')
BD_BEFORE=$(vpp_bd_ids | tr '\n' ' ')
BOND_BEFORE=$(vpp_bond_set | tr '\n' ' ')

# 收尾：把本脚本可能留下的对象删掉（正常路径下已删过；失败/中断时兜底）。
# 只在**配置里确实还有**时才发删除，避免无谓的「无匹配配置」噪音。
cleanup() {
  local cfg="$1" todo=""
  for o in "virtual-switches $V3" "bonds $B3" "port-mirroring $S3"; do
    case "$o" in
      virtual-switches*) grep -q "^virtual-switches ${o##* } \{" "$cfg" 2>/dev/null && todo="$todo|${o}";;
      bonds*)            grep -q "^bonds ${o##* } \{" "$cfg" 2>/dev/null && todo="$todo|${o}";;
      port-mirroring*)   grep -q "^port-mirroring ${o##* } \{" "$cfg" 2>/dev/null && todo="$todo|${o}";;
    esac
  done
  [ -z "$todo" ] && return 0
  {
    echo configure
    printf '%s\n' "$todo" | tr '|' '\n' | grep -v '^$' | sed 's/^/delete /'
    echo commit
  } > /tmp/cli-lifecycle-clean-$$.txt
  "$CLI_BIN" -server "$SRV" -u admin -p "$PW" -source console -c "$(cat /tmp/cli-lifecycle-clean-$$.txt)" >/dev/null 2>&1
  rm -f /tmp/cli-lifecycle-clean-$$.txt
  echo "· 收尾：已删除本脚本留下的对象"
}
trap 'cleanup "$BASE"' EXIT

echo "==================== CLI 生命周期与组合检查（删除/重启/跨对象）===================="
echo "服务端 $SRV ｜ CLI $CLI_BIN ｜ 对象前缀 $PFX ｜ 基线配置 $BASE"
cleanup "$BASE"          # 上一次异常中断可能留下的对象，先清掉，保证本遍可重复跑

# ============================================================ L1
hdr "L1 删对象后回读（配置 + 运行态双读）"
FREE_LIST=$(free_ports | tr '\n' ' ')
P1=$(free_ports | sed -n 1p); P2=$(free_ports | sed -n 2p)
FREE_N=$(free_ports | grep -c . || true)
echo "    可自由分配的数据面口: ${FREE_LIST:-（无）}（管理口与已有角色的口都排除在外）"

# ---- L1-1 删 L3 交换机（含 L3 接口地址与静态路由）：配置残留 + 数据面残留 ----
if [ -z "$P1" ]; then
  skip "L1-1 删 L3 交换机后无残留 —— 没有可自由分配的数据面口（不能动管理口与基线角色的口）"
else
  echo "    对象 $V3 ｜ L3 接口 $P1 $ADDR3 ｜ 静态路由 $ROUTE3 next-hop $NH3"
  out=$(cli "configure
set virtual-switches $V3 type l3
set virtual-switches $V3 l3-interface $P1 ip address $ADDR3
set virtual-switches $V3 static-routes $ROUTE3 next-hop $NH3
commit")
  if cli_err "$out"; then
    if printf '%s' "$out" | grep -q '角色冲突'; then
      skip "L1-1 删 L3 交换机后无残留 —— 守卫正确拒绝（$P1 在配置里另有角色），本机没有第二个自由口"
    else
      bad "L1-1 建 L3 交换机失败，删除路径无从检验"
      printf '%s\n' "$out" | sed 's/^/      | /' | head -6
    fi
  else
    # 建立侧正向控制：三件事都要看得见（配置、表、接口地址），否则删除侧断言没有意义
    c1=$(cli "show configuration" | grep -c "^virtual-switches $V3 {" || true)
    tid=$(vpp_table_id_of "$V3"); l3=$(vpp_if_l3 "$P1")
    echo "    建立后: 配置条目 $c1 ｜ VPP 表 ${V3}${tid:+ table_id=$tid} ｜ $P1 的 L3: ${l3:-（无）}"
    if [ "$c1" -ge 1 ] && [ -n "$tid" ] && printf '%s' "$l3" | grep -q "table-id $tid"; then
      ok "L1-1 建立侧正向控制（配置+VPP 表+接口地址都到位）"
      del=$(cli "configure
delete virtual-switches $V3
commit")
      if cli_err "$del"; then
        bad "L1-1 删除 $V3 时报错，配置与运行态都可能停在中间态"
        printf '%s\n' "$del" | sed 's/^/      | /' | head -6
      fi
      cli "show configuration" > /tmp/cli-lifecycle-after-$$.txt
      if diff -q "$BASE" /tmp/cli-lifecycle-after-$$.txt >/dev/null; then
        ok "L1-1 删除后配置与建之前**逐字节一致**（同名 VRF 条目未残留）"
      else
        exp_act "配置与 $BASE 逐字节一致" "有差异"
        diff "$BASE" /tmp/cli-lifecycle-after-$$.txt | head -8 | sed 's/^/      | /'
        bad "L1-1 删除后配置有残留"
      fi
      rm -f /tmp/cli-lifecycle-after-$$.txt
      if cli "show vrfs" | grep -q "$V3"; then
        bad "L1-1 删除后 show vrfs 仍列出 $V3"
      else
        ok "L1-1 删除后 show vrfs 不再列出（配置侧回读干净）"
      fi
      if [ -n "$(vpp_if_l3 "$P1")" ]; then
        exp_act "$P1 在 VPP 里不再有 L3 地址" "$(vpp_if_l3 "$P1")"
        bad "L1-1 删除后数据面仍把 L3 地址留在 $P1 上"
      else
        ok "L1-1 删除后 $P1 在 VPP 里的 L3 地址已撤"
      fi
      # 运行态残留（R84-29）：产品必须**先解绑再删表**——接口仍绑着表时 VPP 的
      # ip_table_add_del(del) 返回成功却不删（表项继续留在 show ip table 里带
      # locks:[interface:…]，直到 VPP 重启），只信返回码就是「报成功却没做到」。
      # 故这里按断言判定（此前是登记）：表不得再出现，且该口不得再带非默认 table-id。
      rid=$(vpp_table_id_of "$V3"); rid6=$(vpp_table_id_of6 "$V3")
      if [ -n "$rid" ] || [ -n "$rid6" ]; then
        exp_act "$V3 的表从 show ip table / show ip6 table 消失" "IPv4 ${rid:-已消失} ｜ IPv6 ${rid6:-已消失}"
        [ -n "$rid" ] && note "残留 IPv4 表的 VPP 详情：$(vppctl show ip fib table "$rid" 2>/dev/null | tr -d '\r' | head -1)"
        bad "L1-1 删除后 VPP 里仍留着 $V3 的空表（删表未生效——接口未解绑时 VPP 返回成功却不删）"
      else
        ok "L1-1 删除后 v4/v6 两张表都已从 VPP 消失（解绑→删表确实生效，不再是空表滞留）"
      fi
      # 旁证：「回到默认表」的直接体现是接口所属的表——有地址时地址行会打印 table-id，
      # 无地址的口 VPP 不打印该字段，故与上一条（表已消失）合起来作判据，不单独立论。
      pl3=$(vpp_if_l3 "$P1")
      if printf '%s' "$pl3" | grep -q 'table-id [1-9]'; then
        exp_act "$P1 回到默认表（地址行不带非默认 table-id）" "$pl3"
        bad "L1-1 删除后 $P1 仍绑在非默认表里（该口没解绑）"
      else
        ok "L1-1 删除后 $P1 已回到默认表（地址行无非默认 table-id：${pl3:-该口已无 L3 地址}）"
      fi
      if [ -n "$rid" ] && vppctl show ip fib table "$rid" 2>/dev/null | tr -d '\r' | grep -qF "$ROUTE3"; then
        exp_act "该对象声明的 $ROUTE3 随对象一起从 VPP 表内撤销" "仍在 $V3 的表里"
        bad "L1-1 删除后数据面仍留着该对象声明的静态路由（撤销缺口）"
      else
        ok "L1-1 数据面里该对象声明的静态路由已撤销"
      fi
    else
      exp_act "配置条目≥1、VPP 有表、$P1 的地址在那张表里" "配置 $c1 ｜ 表 ${tid:-无} ｜ 地址行 ${l3:-无}"
      bad "L1-1 建立侧正向控制不成立，删除路径无从判定（本项不继续）"
    fi
  fi
fi

# ---- L1-2 删 bond：VPP 里的 BondEthernet 是否残留（撤销缺口） ----
if [ -z "$P1" ]; then
  skip "L1-2 删 bond 后无残留 —— 没有可自由分配的数据面口"
else
  out=$(cli "configure
set bonds $B3 members 0 $P1
commit")
  after_create=$(vpp_bond_set | tr '\n' ' ')
  if cli_err "$out" || [ "$after_create" = "$BOND_BEFORE" ]; then
    exp_act "建立 bond 后 VPP 的 bond 集合比之前多一个" "之前 $BOND_BEFORE ｜ 之后 $after_create"
    bad "L1-2 建立 bond 失败或未落数据面，删除路径无从检验"
    printf '%s\n' "$out" | sed 's/^/      | /' | head -6
  else
    ok "L1-2 建立侧正向控制（VPP bond 集合 $BOND_BEFORE→$after_create）"
    del=$(cli "configure
delete bonds $B3
commit")
    after_del=$(vpp_bond_set | tr '\n' ' ')
    if cli_err "$del"; then
      bad "L1-2 删除 $B3 时报错"
      printf '%s\n' "$del" | sed 's/^/      | /' | head -6
    fi
    echo "    删除后 VPP bond 集合: ${after_del:-（空）}；成员口: $(vpp_bond_members | tr '\n' ' ')"
    if [ "$after_del" = "$BOND_BEFORE" ]; then
      ok "L1-2 删除后 VPP 里无 BondEthernet 残留（集合回到删除前）"
    else
      exp_act "VPP bond 集合 $BOND_BEFORE" "$after_del"
      bad "L1-2 删除后 VPP 里仍有 BondEthernet 残留"
    fi
    if vpp_bond_members | grep -qx "$P1"; then
      bad "L1-2 删除后 $P1 仍是某个 bond 的成员"
    else
      ok "L1-2 删除后 $P1 已回普通口"
    fi
    cli "show configuration" > /tmp/cli-lifecycle-after-$$.txt
    if diff -q "$BASE" /tmp/cli-lifecycle-after-$$.txt >/dev/null; then
      ok "L1-2 删除后配置与建之前逐字节一致"
    else
      diff "$BASE" /tmp/cli-lifecycle-after-$$.txt | head -8 | sed 's/^/      | /'
      bad "L1-2 删除后配置有残留"
    fi
    rm -f /tmp/cli-lifecycle-after-$$.txt
  fi
fi

# ---- L1-3 删端口镜像会话：会话是否真的从数据面摘掉 ----
if [ -z "$P1" ] || [ -z "$P2" ]; then
  skip "L1-3 删端口镜像会话后无残留 —— 需要两个自由口（源口 + 分析口），本机只有 $FREE_N 个（管理口是红线，不拿它试验）"
else
  out=$(cli "configure
set port-mirroring $S3 source interface $P1 direction both
set port-mirroring $S3 analyzer interface $P2
commit")
  if cli_err "$out"; then
    skip "L1-3 端口镜像会话 —— 声明被拒（$P1/$P2 之一另有角色），本机没有两个自由口"
    printf '%s\n' "$out" | grep -v '^$' | sed 's/^/      | /' | head -4
  else
    # 建立侧：CLI 视图 + VPP 侧（`show interface span` 是本版本 VPP 的 span 插件视图，
    # 若该子命令不可用则只作登记——判据不押在没核对过的输出形状上）
    seen=$(cli "show port-mirroring" | grep -c "$S3" || true)
    vspan=$(vppctl show interface span 2>&1 | tr -d '\r')
    echo "    CLI 视图里的会话数 $seen ｜ VPP span 视图: $(printf '%s' "$vspan" | head -3 | tr '\n' ' ')"
    if [ "$seen" -ge 1 ]; then ok "L1-3 建立侧正向控制（CLI 视图能看到该会话）"
    else bad "L1-3 建立后 CLI 视图看不到该会话"; fi
    case "$vspan" in *unknown*|"") note "VPP 侧 span 视图不可用，运行态对照缺席（仅登记）";; esac
    del=$(cli "configure
delete port-mirroring $S3
commit")
    if cli_err "$del"; then
      bad "L1-3 删除 $S3 时报错"
      printf '%s\n' "$del" | sed 's/^/      | /' | head -6
    fi
    if cli "show port-mirroring" | grep -q "$S3"; then
      bad "L1-3 删除后 CLI 视图仍列出该会话"
    else
      ok "L1-3 删除后 CLI 视图里已无该会话"
    fi
    cli "show configuration" > /tmp/cli-lifecycle-after-$$.txt
    if diff -q "$BASE" /tmp/cli-lifecycle-after-$$.txt >/dev/null; then
      ok "L1-3 删除后配置与建之前逐字节一致"
    else
      diff "$BASE" /tmp/cli-lifecycle-after-$$.txt | head -8 | sed 's/^/      | /'
      bad "L1-3 删除后配置有残留"
    fi
    rm -f /tmp/cli-lifecycle-after-$$.txt
  fi
fi

# BD 成员还原：L1 全程不该改动任何既有交换机的成员
hdr "L1-4 既有交换机的 BD 成员未被本段改动"
bd_after=$(vpp_bd_ids | tr '\n' ' ')
if [ "$bd_after" = "$BD_BEFORE" ]; then ok "BD 集合与开工前一致（${bd_after:-空}）"
else exp_act "BD 集合 $BD_BEFORE" "$bd_after"; bad "BD 集合变了（本段建删对象留下了痕迹）"; fi

# ============================================================ L3
hdr "L3 组合能力（VNF 的 vNIC ↔ 它声明的交换机/BD，条件执行；**先跑**——趁邻居表/会话还是热的）"
vms=$(cli "show virtual-machine-functions" | awk 'NR > 1 && $1 !~ /^（/ {print $1}')
if [ -z "$vms" ]; then
  skip "L3-1 vNIC 是否真的接进交换机 —— 没有 VNF 对象（无对象可对照）"
else
  for vm in $vms; do
    detail=$(cli "show virtual-machine-functions $vm detail")
    # 声明侧：interfaces <nic> { … virtual-switch <vs> … }（块内取名与归属，块结束即出栈）
    pairs=$(printf '%s\n' "$detail" | awk '
      /^interfaces [A-Za-z0-9._-]+ \{/ { nic=$2; vs=""; inb=1; next }
      inb && /^\}/ { if (nic != "" && vs != "") print nic, vs; inb=0; next }
      inb && /virtual-switch / { vs=$2; sub(/;.*/, "", vs) }
    ')
    if [ -z "$pairs" ]; then
      skip "L3-1 $vm —— 该 VNF 没有声明 virtual-switch 的接口（无从组合）"
      continue
    fi
    # 逐**行**读（每行是「接口名 交换机名」）；用 here-string 而不是管道，
    # 否则 while 在子 shell 里跑，计数（PASS/FAIL）出不来。
    while read -r nic vs; do
      [ -z "$nic" ] || [ -z "$vs" ] && continue
      ifn="vh-$vm-$nic"
      if ! vpp_ifaces | grep -qx "$ifn"; then
        exp_act "$ifn 出现在 VPP 接口清单里" "不在"
        bad "L3-1 $vm 的 $nic 声明归 $vs，但 VPP 里没有 $ifn（vNIC 未落地）"
        continue
      fi
      bdid=$(vpp_bd_id_of_tag "$vs")
      if [ -n "$bdid" ]; then         # L2 交换机：成员表里必须有它（否则 guest 静默失去 L2 连通）
        if vpp_bd_members "$bdid" | grep -qx "$ifn"; then
          ok "L3-1 $ifn 在该交换机的 BD 成员里（$vs bd-id=$bdid）"
        else
          exp_act "$ifn 出现在 $vs 的 BD 成员表" "成员: $(vpp_bd_members "$bdid" | tr '\n' ' ')"
          bad "L3-1 $ifn 从未挂进 $vs 的 bridge-domain（guest 拿不到地址，且全程零报错）"
        fi
      else                             # L3 交换机：看它属于哪张表（地址行里的 table-id）
        tid=$(vpp_table_id_of "$vs"); l3=$(vpp_if_l3 "$ifn")
        if [ -z "$tid" ]; then
          bad "L3-1 $vs 在 VPP 里没有对应的表（L3 交换机未落地）"
        elif [ -n "$l3" ] && printf '%s' "$l3" | grep -q "table-id $tid"; then
          ok "L3-1 $ifn 落在 $vs 的表里（$l3）"
        else
          exp_act "$ifn 的 L3 行含 table-id $tid" "${l3:-（该口没有 L3 行）}"
          bad "L3-1 $ifn 不在 $vs 的表里（L3 侧没接上）"
        fi
      fi
      # 正向控制（决策 #334）：ping **该转发域**里的主机（对端地址只认**邻居表**这个独立
      # 事实源；没有就如实标「不可判定」——绝不猜地址，猜错会把「对端不在」判成「产品不通」）。
      # 域 table-id 动态推导（**禁写死现场值**，每一步都有独立事实源）：
      #   ① L3 交换机 → 同名表（`show ip table` 输出末列即表名，按名反查）；
      #   ② L2 交换机带 BVI 网关 → 产品为网关专属建的 VRF「vr-<交换机名>」同名表
      #      （internal/orchestrator/network/l3.go GatewayVRFName；真机实测
      #      `table_id:1445873 vr-sem-vs`，表名不带 vr- 前缀外的任何修饰）；
      #   ③ 退路 → vhost 自己地址行上的 table-id（vNIC 作 L3 接口，决策 #172）；
      #   ④ 都取不到 → 空串＝表 0 口径（只认地址行不带 table-id 的口）。
      if [ -n "$bdid" ]; then dom_tid=$(vpp_table_id_of "vr-$vs"); else dom_tid="$tid"; fi
      [ -z "$dom_tid" ] && dom_tid=$(vpp_if_l3 "$ifn" | sed -n 's/.*table-id *\([0-9][0-9]*\).*/\1/p')
      dom_peers=$(vpp_domain_neighbors "$dom_tid" "$ifn")
      # 优先认 vhost 自己行上的邻居——能从 vhost 发的 ping 才是 vNIC 侧数据面的正控；
      # 没有再退到同域**别的口**（如 BVI 网关）行上的邻居（同一转发域的主机同样有效）。
      peer=$(printf '%s\n' "$dom_peers" | awk -v i="$ifn" '$2 == i {print $1" "$2; exit}')
      [ -z "$peer" ] && peer=$(printf '%s\n' "$dom_peers" | sed -n 1p)
      if [ -z "$peer" ]; then
        skip "L3-2 $ifn 的转发域连通性 —— 邻居表里没有该域的任何主机（先让 guest 发一次流量，ARP 才会有它）"
      else
        set -- $peer; peer_ip="$1"; psrc="$2"
        # ping 源：vhost 带地址（vNIC 作 L3 接口）就从 vhost 发；vhost 无地址（BD 成员，
        # VPP ping 无源可用）时只能从对端所在口的 L3 身份发（如 BVI 网关）——此时证明的是
        # 转发域连通（vhost 是否真在域里已由 L3-1 的 BD 成员断言背书），输出里如实说明。
        src="$ifn"; [ -z "$(vpp_if_l3 "$ifn")" ] && src="$psrc"
        pout=$(vppctl ping "$peer_ip" source "$src" repeat 5 2>&1 | tr -d '\r')
        sent=$(printf '%s' "$pout" | sed -n 's/.*Statistics: *\([0-9][0-9]*\) sent.*/\1/p')
        recv=$(printf '%s' "$pout" | sed -n 's/.*Statistics: *[0-9][0-9]* sent, *\([0-9][0-9]*\) received.*/\1/p')
        echo "    ping $peer_ip source $src（对端取自转发域 table-id=${dom_tid:-0} 的邻居表，见 $psrc）→ $(printf '%s' "$pout" | grep Statistics: | head -1)"
        if [ "${sent:-0}" -eq 0 ]; then
          skip "L3-2 $ifn 到主机 $peer_ip 的连通性 —— 0 发包（本机到该地址没有可用路径）"
        elif [ "${recv:-0}" -gt 0 ]; then
          ok "L3-2 正向控制成立：经 $src 能 ping 通 $peer_ip（$recv/$sent 应答）"
        else
          exp_act "至少 1 个应答" "$sent 发包 0 应答"
          bad "L3-2 经 $src ping $peer_ip 无应答（vNIC 侧数据面不通）"
        fi
      fi
    done <<< "$pairs"
  done
fi

# L3-3 NAT 会话：CLI 的 show nat 与 VPP 的会话表逐条对账（答非所问会在这里露出来）
if ! cli "show configuration" | grep -q '^nat {'; then
  skip "L3-3 NAT 会话对账 —— 配置里没有 NAT（无对象可对照）"
else
  cli_n=$(cli "show nat" | awk '$1 ~ /^[0-9]+\.[0-9]/ {n++} END {print n+0}')
  vpp_n=$(vpp_nat_sessions)
  echo "    CLI show nat 会话条数=$cli_n；VPP nat44 ei 会话条数=$vpp_n"
  if [ "$cli_n" -eq 0 ] && [ "$vpp_n" -eq 0 ]; then
    skip "L3-3 NAT 会话对账 —— 两侧都空（本次没有 inside 主机发包，产生不了会话）"
  elif [ "$cli_n" -eq "$vpp_n" ]; then
    ok "L3-3 两侧会话条数一致（$cli_n 条）"
  else
    exp_act "CLI 与 VPP 会话条数一致" "CLI $cli_n vs VPP $vpp_n"
    bad "L3-3 NAT 会话条数对不上（可能取自配置/按错用户查 dump）"
  fi
fi

# ============================================================ L2
hdr "L2 重启/重放一致性（重启 $VPP_UNIT，**不**重启 $NFVIS_UNIT；**最后跑**——重启会清空 ARP 与会话表，跑完机器停在收敛态）"
if [ -z "$P1" ]; then
  skip "L2 全部 —— 没有可自由分配的数据面口，统计断言没有对象"
elif ! systemctl list-unit-files 2>/dev/null | grep -q "^$VPP_UNIT\.service"; then
  skip "L2 全部 —— 本机没有 $VPP_UNIT.service（VPP 不由 systemd 托管）"
else
  echo "    ⚠️ 即将重启 VPP：数据面会短暂中断（业务口不通，guest 可能掉线再恢复）"
  TS=$(date '+%Y-%m-%d %H:%M:%S')
  nat_before=$(vpp_nat_ifaces | tr '\n' ' ')
  echo "    重启前: NAT 接口 ${nat_before:-（无）} ｜ BD $(vpp_bd_ids | tr '\n' ' ')"
  systemctl restart "$VPP_UNIT" >/dev/null 2>&1
  up=no
  for _ in $(seq 1 "$CONV_TIMEOUT"); do
    if vppctl show version >/dev/null 2>&1; then up=yes; break; fi
    sleep 1
  done
  conv=no
  if [ "$up" = yes ]; then
    for _ in $(seq 1 "$CONV_TIMEOUT"); do
      if journalctl -u "$NFVIS_UNIT" --since "$TS" --no-pager 2>/dev/null | grep -q '恢复收敛完成'; then conv=yes; break; fi
      sleep 1
    done
  fi
  sleep 2
  if [ "$up" = yes ]; then ok "L2-0 VPP 重新可连（${CONV_TIMEOUT}s 内）"
  else bad "L2-0 VPP 重启后 ${CONV_TIMEOUT}s 内仍连不上（vppctl 不可用）"; fi
  if [ "$conv" = yes ]; then ok "L2-0 守护进程报告恢复收敛完成"
  else note "未在 ${CONV_TIMEOUT}s 内看到「恢复收敛完成」日志标记（可能只是文案/超时变化，下面按事实判定）"; fi

  # L2-1 统计仍可读：抓「stats 连接只建一次，VPP 重启后永久不可用」
  st=$(cli "show interfaces $P1 statistics")
  echo "    CLI: $(printf '%s' "$st" | head -1)"
  echo "    VPP: $P1 rx packets=$(vpp_if_rx "$P1") rx bytes=$(vpp_if_rxbytes "$P1")"
  if cli_err "$st"; then
    bad "L2-1 VPP 重启后 $P1 的统计取不到（重启前同一命令有值——连接陈旧未重连）"
  else
    nums=$(printf '%s' "$st" | sed -n 's/.*statistics: *{\([0-9 ]*\)}.*/\1/p')
    c_rx=$(printf '%s' "$nums" | awk '{print $1}'); c_bytes=$(printf '%s' "$nums" | awk '{print $3}')
    v_rx=$(vpp_if_rx "$P1"); v_bytes=$(vpp_if_rxbytes "$P1")
    if [ -z "$nums" ]; then
      bad "L2-1 统计输出不是计数元组，无法对账"
    else
      # 两次读之间可能真有流量，给一个**明确写出来**的容差，而不是"看起来差不多"
      tol_rx=$(( v_rx / 50 + 5 )); tol_b=$(( v_bytes / 50 + 10 ))
      d_rx=$(( c_rx - v_rx )); [ "$d_rx" -lt 0 ] && d_rx=$(( -d_rx ))
      d_b=$(( c_bytes - v_bytes )); [ "$d_b" -lt 0 ] && d_b=$(( -d_b ))
      if [ "$d_rx" -le "$tol_rx" ] && [ "$d_b" -le "$tol_b" ]; then
        ok "L2-1 VPP 重启后统计可用且与 VPP counters 一致（rx $c_rx vs $v_rx，容差 $tol_rx）"
      else
        exp_act "CLI rx=$v_rx±$tol_rx，rx_bytes=$v_bytes±$tol_b" "CLI rx=$c_rx，rx_bytes=$c_bytes"
        bad "L2-1 统计值与 VPP counters 对不上（是否取自配置/陈旧连接）"
      fi
    fi
  fi

  # L2-2 NAT 重放：配置声明的 inside/outside 必须仍在数据面（进程内状态不随 VPP 失效会漏这一步）
  if [ -z "$nat_before" ]; then
    skip "L2-2 NAT 重放 —— 配置里没有 NAT（本机无该特性，无从判别）"
  else
    nat_after=$(vpp_nat_ifaces | tr '\n' ' ')
    echo "    重启后: NAT 接口 ${nat_after:-（无）}"
    if [ "$nat_after" = "$nat_before" ]; then
      ok "L2-2 NAT 接口集合与重启前一致（${nat_after}）"
    else
      exp_act "NAT 接口 ${nat_before}" "${nat_after:-（空——插件未重放）}"
      bad "L2-2 VPP 重启后 NAT 的 inside/outside 没有回到数据面（重放被跳过）"
    fi
  fi

  # L2-3 日志：重启这段不许有未收敛项（顺序问题会在这里冒出来）
  unc=$(journalctl -u "$NFVIS_UNIT" --since "$TS" --no-pager 2>/dev/null | grep -c 未收敛 || true)
  if [ "$unc" -eq 0 ]; then
    ok "L2-3 重启后日志无「未收敛」（0 条）"
  else
    exp_act "0 条未收敛" "$unc 条"
    journalctl -u "$NFVIS_UNIT" --since "$TS" --no-pager 2>/dev/null | grep 未收敛 | head -3 | sed 's/^/      | /'
    bad "L2-3 重启后日志有未收敛项"
  fi
  echo "    重启后: BD $(vpp_bd_ids | tr '\n' ' ')｜IP 表 $(vpp_tables | tr '\n' ' ')"
fi

# ============================================================ 终态
hdr "终态报告（本脚本为机器留下的状态）"
cli "show configuration" > /tmp/cli-lifecycle-final-$$.txt
if diff -q "$BASE" /tmp/cli-lifecycle-final-$$.txt >/dev/null; then
  ok "本脚本建的对象已清干净（配置与开工前逐字节一致）"
else
  exp_act "配置与 $BASE 一致" "有差异"
  diff "$BASE" /tmp/cli-lifecycle-final-$$.txt | head -10 | sed 's/^/      | /'
  bad "配置与开工前不一致（本脚本留下的对象没清干净）"
fi
rm -f /tmp/cli-lifecycle-final-$$.txt
echo "    数据面: BD $(vpp_bd_ids | tr '\n' ' ')｜IP 表 $(vpp_tables | tr '\n' ' ')｜bond $(vpp_bond_set | tr '\n' ' ')"
echo "    NAT 接口 $(vpp_nat_ifaces | tr '\n' ' ')（开工前 $NFVIS_PASS_BEFORE）"
nat_after_final=$(vpp_nat_ifaces | tr '\n' ' ')
if [ "$nat_after_final" = "$NFVIS_PASS_BEFORE" ]; then
  ok "既有基线的 NAT 拓扑与开工前一致，未被本脚本改动"
else
  exp_act "NAT 接口 $NFVIS_PASS_BEFORE" "$nat_after_final"
  bad "既有基线的 NAT 拓扑变了（重放没恢复或不慎改到）"
fi
echo "    VNF: $(cli "show virtual-machine-functions" | awk 'NR > 1 && $1 !~ /^（/ {printf "%s(%s) ", $1, $2}')"

rm -f "$BASE"          # 基线快照只在本次运行里有意义，别在机器上堆 /tmp 垃圾（收尾 trap 仍能安全重入）
echo
echo "==================== 合计：通过 $PASS / 失败 $FAIL / 跳过（不可判定）$SKIP / 登记 $INFO ===================="
if [ "$SKIP" -gt 0 ]; then
  echo "⚠️ 有 $SKIP 项**跳过（不可判定）**——它们既不算通过也不算失败，本遍没测到；"
  echo "   要么补前置（自由口/流量/guest 地址），要么在报告里如实写成「未覆盖」。"
fi
if [ "$FAIL" -gt 0 ]; then
  echo "⚠️ 有失败项：请先确认判据本身成立（期望 vs 实际已打印），再改实现或改断言"
  exit 1
fi
echo "（L2 重启过 VPP：若此后发现某对象没回到数据面，可先 systemctl restart $NFVIS_UNIT 触发重放）"
