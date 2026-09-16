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
# ⚠️ 自身也会出错，故必须**自校准**：oracle 取错、断言写错都会制造假红/假绿，而假绿比没有测试更危险。
#    修脚本后，请拿「已知正确」的实现确认判 PASS、拿「已知错误」的确认判 FAIL（红-绿）。
#    已知坑（都已在本脚本里规避，勿退回去）：
#      · 终端文本解析脆弱 → 候选一律走 REST（JSON），不 sed 终端输出；
#      · 输出格式会变（本脚本会随契约调整）→ 解析要有兜底并打印原始片段；
#      · 扰动必须**真的改变**事实源：用确定空闲的 BD id、把接口翻到**相反**状态，
#        并先确认「扰动前后 oracle 值不同」，否则本项应报「无从判别」而不是 PASS。
#
# 前置（缺一不可，均在 nfvis-vm 上）：
#   1) 开发态 nfvisd 已起：/tmp/nfvisd -db /tmp/nfvis-cli.db -listen 127.0.0.1:18443 \
#        -init-admin-password "Admin@12345" -allow-plaintext
#   2) /tmp/nfvis-cli 为**同版本**构建产物；口令 Admin@12345
#   3) VPP 运行中（vppctl 可用）；内核侧读 /sys/class/net；libvirt/Docker 可选（缺失则该项降级为登记）
#
# 用法：
#   bash contrib/scripts/cli-semantic-check.sh
# 退出码：有失败项 → 1（可用作发布前门槛）。
#
# 副作用与恢复：脚本会临时改 VPP（建/删一个 BD、翻转某接口 admin 状态、在**开发态实例**里
# 建一个交换机并删除）。结束时会尽力恢复；仍建议随后 `systemctl restart vpp` + `restart nfvis`
# 以清掉残留拓扑（脚本会打印提醒）。
set -u

SRV=${SRV:-http://127.0.0.1:18443}
CLI_BIN=${CLI_BIN:-/tmp/nfvis-cli}
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

TOKEN=$(curl -s -X POST "$SRV/api/v1/login" -H 'Content-Type: application/json' \
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
  curl -s "$SRV/api/v1/cli/candidates?tokens=$1&partial=" -H "Authorization: Bearer $TOKEN" \
    | tr ',' '\n' | sed -n 's/.*"Token":"\([^"]*\)".*/\1/p' | sort
}

# ---------- oracles（独立事实源）----------
vpp_ifaces()  { vppctl show interface 2>/dev/null | awk 'NR>1 && $2 ~ /^[0-9]+$/ {print $1}' | grep -v '^local0$' | sort; }
vpp_state()   { vppctl show interface 2>/dev/null | awk -v n="$1" '$1==n && $2 ~ /^[0-9]+$/ {print $3}'; }
vpp_bd_ids()  { vppctl show bridge-domain 2>/dev/null | awk 'NR>1 && $1 ~ /^[0-9]+$/ {print $1}' | sort; }
vpp_bd_tag()  { vppctl show bridge-domain "$1" detail 2>/dev/null | sed -n 's/.*BD-Tag: //p'; }
vpp_bd_tags() { for id in $(vpp_bd_ids); do vpp_bd_tag "$id"; done | grep -v '^$' | sort; }
vpp_l2fib_count() {
  # 空表时 vppctl 打印 "no l2fib entries"（按行数计会得 1，造成假红）
  local out; out=$(vppctl show l2fib 2>/dev/null)
  case "$out" in *"no l2fib entries"*) echo 0; return;; esac
  printf '%s
' "$out" | awk 'NR>1 && NF>0 {n++} END {print n+0}'
}
kernel_phys() { for n in /sys/class/net/*; do [ -e "$n/device" ] && basename "$n"; done | sort; }
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

PASS=0; FAIL=0; INFO=0
ok()   { echo "  ✓ $1"; PASS=$((PASS+1)); }
bad()  { echo "  ✗ $1"; FAIL=$((FAIL+1)); }
note() { echo "  · $1"; INFO=$((INFO+1)); }
hdr()  { echo; echo "===== $1"; }
cmp_sets() { # cmp_sets <标签> <期望> <实际>
  local label="$1" exp="$2" got="$3" miss=""
  for i in $exp; do case " $got " in *" $i "*) ;; *) miss="$miss $i";; esac; done
  echo "    期望: ${exp:-（空）}"; echo "    实际: ${got:-（空）}"
  if [ -z "$miss" ]; then ok "$label"; else bad "$label —— 缺:$miss"; fi
}

echo "==================== CLI 语义校验（结果对不对）===================="
echo "服务端 $SRV ｜ CLI $CLI_BIN ｜ 标记 $MARK"

# ============ 准备：被测接口（后续多项需要配置里有对象）============
IFACE=$(vpp_ifaces | head -1)
if [ -z "$IFACE" ]; then echo "✗ VPP 中无接口，无法继续"; exit 1; fi
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
  else bad "$sub 无任何状态/计数字段（契约 §1.1 要求；疑似回落到配置 dump）"; fi
done

hdr "S5 接口「链接状态」：静态列名 + **扰动判别**（契约要求 驱动/链接状态/速率）"
hdrrow=$(cli "show interfaces physical" | head -1)
echo "    表头: $hdrrow"
if echo "$hdrrow" | grep -qiE 'driver|link|speed|驱动|链接状态|速率'; then
  ok "表头含驱动/链接状态/速率类字段"
else
  bad "表头无驱动/链接状态/速率列（契约 §1.1 明列）"
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

hdr "S7 管道 display 支持面（契约 §3 曾声明 | display set）"
pipeout=$(cli "configure
show | display set")
if echo "$pipeout" | grep -q '未实现'; then
  ok "| display set 未实现但**已登记**且给出替代路径（附录 A #84）"
elif echo "$pipeout" | grep -q '仅支持 json|xml'; then
  bad "| display set 仍是裸的「仅支持 json|xml」——契约未更正或未登记"
else
  note "| display set 行为未知：$(echo "$pipeout" | head -1)"
fi

hdr "S8 MAC 表 ↔ VPP l2fib 条数（对照，两者都空也算一致）"
l2vs=$(cli_bdnames | tr ' ' '\n' | grep -vE '^-$|^$' | head -1)
if [ -n "$l2vs" ]; then
  cliout=$(cli "show virtual-switches $l2vs mac-table")
  if echo "$cliout" | grep -q 'MAC 表为空'; then cli_n=0
  else cli_n=$(echo "$cliout" | awk 'NR>1 && $1 ~ /:/ {n++} END {print n+0}'); fi
  fib_n=$(vpp_l2fib_count)
  echo "    CLI mac-table 条数=$cli_n；VPP l2fib 行数=$fib_n"
  if [ "$fib_n" -eq 0 ] && [ "$cli_n" -eq 0 ]; then ok "两侧一致（均为空，本环境无流量）"
  elif [ "$cli_n" -eq "$fib_n" ]; then ok "两侧一致（$cli_n 条）"
  else bad "条数不一致（CLI $cli_n vs VPP $fib_n）——需人工判读（可能含子接口/其他 BD 的条目）"; fi
else note "无交换机对象，本项略"; fi

hdr "S9 运行态对象是否被视图反映（诊断，不计失败）"
libv=$(virsh list --all 2>/dev/null | awk 'NR>2 && $2!="" {print $2}' | tr '\n' ' ')
dockerps=$(docker ps --format '{{.Names}}' 2>/dev/null | tr '\n' ' ')
clivm=$(cli "show virtual-machine-functions" | grep -oE '^[a-zA-Z0-9._-]+' | tr '\n' ' ')
clict=$(cli "show container-functions" | grep -oE '^[a-zA-Z0-9._-]+' | tr '\n' ' ')
echo "    libvirt 域: ${libv:-（无）} ｜ CLI VM 列表: ${clivm:-（空）}"
echo "    Docker 容器: ${dockerps:-（无）} ｜ CLI 容器列表: ${clict:-（空）}"
[ -n "$libv" ] && note "libvirt 有域而 CLI 未列出（配置驱动；契约对 VM 列表口径未明确，仅登记）"
[ -n "$dockerps" ] && note "Docker 有容器而 CLI 未列出（同上，仅登记）"
case " $clivm " in *" br0 "*) : ;; esac

# ============ 清理本脚本创建的对象 ============
cli "configure
delete interfaces $IFACE description
commit" >/dev/null 2>&1
if [ "${vs:-}" = "semcheck-$MARK" ]; then
  cli "configure
delete virtual-switches semcheck-$MARK
commit" >/dev/null 2>&1
fi
vppctl create bridge-domain "$PERTURB_BD" del >/dev/null 2>&1

echo
echo "==================== 合计：通过 $PASS / 失败 $FAIL / 登记 $INFO ===================="
echo "（修复/复核后请对比上一次结果；失败项需人工判读契约与 oracle 后再改实现或改断言）"
if [ "$FAIL" -gt 0 ]; then
  echo "⚠️ 有失败项：若为扰动/环境所致，请先确认 oracle 侧确实变化（本脚本已尽量自证）"
  echo "⚠️ 建议随后: systemctl restart vpp && systemctl restart nfvis（清残留拓扑）"
  exit 1
fi
echo "建议随后: systemctl restart vpp && systemctl restart nfvis（清残留拓扑）"
