#!/usr/bin/env bash
# 共用 harness：CLI 全功能测试。
# 失败判定：**行首**出现 % / %% 错误，或**行首**是 "校验失败"。
# 说明：CLI 脚本模式(%%)会中止；配置模式的行内错误是单 %，须显式匹配行首 %。
# ⚠️ 「校验失败」必须**锚定行首**（发现 #9）：产品只在输出开头给「校验失败（未提交）/（candidate 保留）」，
#    而 `show log audit` 一类命令会**回显历史**——某条旧审计记录的 detail 里就可能含「校验失败: …」，
#    不锚定就会把「命令本身成功、只是回显了历史」判成失败（同一条命令在不同审计历史下结论不同）。
#    低置信度的做法是「按输出内容猜」，正确做法是只认产品在**行首**给出的判定。
# 目标与凭据都可经环境变量覆盖——**已装实例无需再打补丁副本**：
#   SRV=https://127.0.0.1:443 CLI_BIN=/usr/bin/nfvis-cli NFVIS_PASSWORD='…' bash cli-fulltest.sh
# （CLI 自身按缺省固定本机证书，故 HTTPS 下不必额外给 CA；缺省值仍是开发态实例。）
SRV=${SRV:-http://127.0.0.1:18443}
CLI_BIN=${CLI_BIN:-/tmp/nfvis-cli}
CLI="$CLI_BIN -server $SRV -u admin"
export NFVIS_PASSWORD=${NFVIS_PASSWORD:-Admin@12345}
# 阶段 2 会声明 API 端口；对已装实例跑时不该把开发态端口写进配置，故参数化
API_PORT=${API_PORT:-18443}
LOG=${LOG:-/tmp/cli-test/full.log}
mkdir -p /tmp/cli-test

PASS=0; FAIL=0; FAILED_LIST=()

_is_fail() { # stdin: 输出；返回 0 表示失败
  grep -qE '^%%|^%[^%]|^%$|^校验失败|^%% '
}

run() { # run <阶段id> <命令>
  local phase="$1"; shift
  local cmd="$*" out rc
  out=$($CLI -c "$cmd" 2>&1); rc=$?
  out=$(printf '%s\n' "$out" | sed '1{/^连接 /d;}')
  if printf '%s\n' "$out" | _is_fail || [ $rc -ne 0 ]; then
    FAIL=$((FAIL+1))
    local brief; brief=$(printf '%s\n' "$out" | grep -nE '^%%|^%[^%]|^%$|^校验失败' | head -2 | tr '\n' ' ')
    FAILED_LIST+=("[$phase] $cmd :: $brief")
    printf '  ✗ %s\n' "$cmd"
    printf '%s\n' "$out" | sed 's/^/       | /' | head -8
  else
    PASS=$((PASS+1))
    printf '  ✓ %s\n' "$cmd"
  fi
  printf '=== [%s] %s\n%s\n\n' "$phase" "$cmd" "$out" >> "$LOG"
}

summary() {
  echo
  echo "############ $1 小结 ############"
  echo "通过 $PASS / 失败 $FAIL / 预期报错 $EXPECTED"
  if [ $FAIL -gt 0 ]; then
    echo "--- 失败清单 ---"
    for f in "${FAILED_LIST[@]}"; do echo "  $f"; done
  fi
}

# expect_fail：**预期报错**的用例（环境受限 / 防呆守卫 / 契约明确拒绝）。
# 与 run 相反：必须出现错误才算通过，否则报「本应被拒但成功了」。
EXPECTED=0
expect_fail() { # expect_fail <阶段> <期望错误里的关键词> <命令>   ← 注意：关键词在前、命令在后
  local phase="$1"; shift
  local want="$1"; shift
  local cmd="$*" out rc
  out=$($CLI -c "$cmd" 2>&1); rc=$?
  out=$(printf '%s\n' "$out" | sed '1{/^连接 /d;}')
  if printf '%s\n' "$out" | grep -q "$want"; then
    EXPECTED=$((EXPECTED+1)); printf '  ⊘ %s（预期报错，含「%s」）\n' "$cmd" "$want"
  else
    FAIL=$((FAIL+1))
    FAILED_LIST+=("[$phase] $cmd :: 本应报「$want」但得到: $(printf '%s' "$out" | head -1)")
    printf '  ✗ %s\n' "$cmd"
    printf '%s\n' "$out" | sed 's/^/       | /' | head -5
  fi
  printf '=== [%s][expect-fail] %s\n%s\n\n' "$phase" "$cmd" "$out" >> "$LOG"
}

# expect_out：**正向内容断言**——输出里必须真的出现关键词，缺失即失败。
# 与 run 的分工：run 只量「命令能不能用」（有值没值都算过），本函数量「答的是不是这件事」，
# 用于套件内**可自证**的内容（如「提交后配置里真能读到刚写的值」「会话清单真带当前会话」）——
# 避免「命令返回 0 但什么也没答」被算作通过（同决策 #89 的判定自洽精神）。
expect_out() { # expect_out <阶段> <关键词> <命令>
  local phase="$1"; shift
  local want="$1"; shift
  local cmd="$*" out rc
  out=$($CLI -c "$cmd" 2>&1); rc=$?
  out=$(printf '%s\n' "$out" | sed '1{/^连接 /d;}')
  if printf '%s\n' "$out" | _is_fail || [ $rc -ne 0 ]; then
    FAIL=$((FAIL+1))
    FAILED_LIST+=("[$phase] $cmd :: 命令失败: $(printf '%s' "$out" | head -1)")
    printf '  ✗ %s（命令失败，无从核对内容）\n' "$cmd"
    printf '%s\n' "$out" | sed 's/^/       | /' | head -5
  elif printf '%s\n' "$out" | grep -qF -- "$want"; then
    PASS=$((PASS+1)); printf '  ✓ %s（输出含「%s」）\n' "$cmd" "$want"
  else
    FAIL=$((FAIL+1))
    FAILED_LIST+=("[$phase] $cmd :: 输出不含「$want」（疑似假成功）")
    printf '  ✗ %s（输出不含「%s」）\n' "$cmd" "$want"
    printf '%s\n' "$out" | sed 's/^/       | /' | head -5
  fi
  printf '=== [%s][expect-out] %s\n%s\n\n' "$phase" "$cmd" "$out" >> "$LOG"
}

# expect_ping_coherent：ping 的**判定自洽**（附录 A #89 / #93）。
#
# 不变量：**未通就不能算通过**。两条支路都必须有 %% 错误——
#   ① 0 发包（VPP 无到达目标的接口/路由）② 发了但无任何应答（100% packet loss）。
# 反过来：真通（有应答）时不该报错。任一条不满足即单列失败项。
#
# 为什么不由脚本「预测」环境（例如看 VPP 有没有 L3 地址就决定 run 还是 expect_fail）：
# 实测同一环境里 `ping <host>`（走 FIB，无路由即 0 发包）与
# `ping <host> source <iface>`（显式出接口，照样发得出去）**结论可以不同**，
# 预测式分流必然误判；本函数只断言「结果与判定自洽」，与环境无关。
expect_ping_coherent() { # expect_ping_coherent <阶段> <命令>
  local phase="$1"; shift
  local cmd="$*" out rc nofail=0
  out=$($CLI -c "$cmd" 2>&1); rc=$?
  out=$(printf '%s
' "$out" | sed '1{/^连接 /d;}')
  if printf '%s
' "$out" | _is_fail || [ "$rc" -ne 0 ]; then
    # 有错误行：必须是「没通」这一类（0 发包 / 无应答），否则是意料之外的报错
    if printf '%s
' "$out" | grep -qE 'Statistics:[[:space:]]*0 sent|received, 100% packet loss|未发出任何报文|无应答'; then
      EXPECTED=$((EXPECTED+1)); printf '  ⊘ %s（未通 → 正确报失败）
' "$cmd"
    else
      FAIL=$((FAIL+1))
      FAILED_LIST+=("[$phase] $cmd :: 报错但不像「未通」（既非 0 发包也非无应答）")
      printf '  ✗ %s（报错原因出乎意料）
' "$cmd"
      printf '%s
' "$out" | sed 's/^/       | /' | head -5
    fi
  else
    # 没报错：那必须**真的通了**（发了且收到应答）
    if printf '%s
' "$out" | grep -qE 'Statistics:[[:space:]]*0 sent'; then
      nofail=1; FAILED_LIST+=("[$phase] $cmd :: 0 发包却未报失败（假绿回归）")
    elif printf '%s
' "$out" | grep -qE 'received, 100% packet loss'; then
      nofail=1; FAILED_LIST+=("[$phase] $cmd :: 无应答却未报失败（未通不算通过）")
    fi
    if [ "$nofail" = 1 ]; then
      FAIL=$((FAIL+1)); printf '  ✗ %s（未通却算通过）
' "$cmd"
      printf '%s
' "$out" | sed 's/^/       | /' | head -5
    else
      PASS=$((PASS+1)); printf '  ✓ %s（真通）
' "$cmd"
    fi
  fi
  printf '=== [%s][ping-coherence] %s
%s

' "$phase" "$cmd" "$out" >> "$LOG"
}

# ==================== 套件收尾清场（决策 #323） ====================
#
# 为什么做：本套件会在**配置库与运行态里真的建对象**（虚拟交换机 vs-l2/vs-l3、ACL acl-test、
# QoS pol-test、VNF cli-vm、容器 cli-ct2、bond0、镜像 cli-test.qcow2/alpine:3.20）。
# 跑完不清场，后续套件或走查就会撞上「同一网口角色互斥」这类**正确校验**而级联出假红
# （round87/round88 各因保留对象出过 12~18 条假红；round102c 实测残留 vs-l3 + acl-test）。
#
# **凭什么认定「某对象是套件的」**（判据写死在这里，见规格书附录 A #323）：
#   ① 固定名且**带套件特征**的对象恒删（幂等；对象不在则跳过）：
#      virtual-switches  vs-l2 / vs-l3       （`vs-` 套件命名空间；阶段 2 提交）
#      acls              acl-test              （`acl-test` 套件命名空间；阶段 2 提交）
#      qos policies      pol-test              （`pol-test` 套件命名空间；阶段 2 提交）
#      virtual-machine-functions cli-vm        （`cli-` 套件命名空间；阶段 3）
#      container-functions       cli-ct2       （`cli-`；阶段 3）
#      images            cli-test.qcow2 / cli-del.qcow2（`cli-`；阶段 3）
#   ② **通用名**对象（bond0、容器镜像 alpine:3.20）可能是用户既有对象，**只删本轮开始前
#      不存在的**——判据是本套件入口拍下的「开始前现场快照」（suite_snapshot_before）；
#      快照缺失时（`--cleanup-only` 且没跑过套件）**一律不删**并如实登记跳过，绝不误删。
#   ③ 打标字段：接口 description == `cli-pre`（阶段 2 写的值）才清、主机名匹配 `cli-tx-`
#      （阶段 6 的值）才还原——**按值识别**，不碰用户自设的描述/主机名。
# 清场**按依赖倒序**（先 VNF/容器，再交换机/ACL/QoS/bond，最后镜像），失败**逐条如实报**。
#
# 口径选择（决策 #323）：采用「**套件入口拍快照 + 末尾自动清场（trap/END 兜底）**」——
# 不要求操作者记得额外调一次收尾命令，中断（Ctrl-C / 中途失败）时 trap 也会清。
# `--cleanup-only` 仅作为**异常中断后的补救入口**存在，不是常规流程的一部分。
SUITE_STATE_DIR=${SUITE_STATE_DIR:-/tmp/cli-test}
SUITE_BEFORE_CFG=${SUITE_BEFORE_CFG:-$SUITE_STATE_DIR/suite-before.cfg}
SUITE_BEFORE_IMG=${SUITE_BEFORE_IMG:-$SUITE_STATE_DIR/suite-before.images}
SUITE_DESC_MARK=${SUITE_DESC_MARK:-cli-pre}
SUITE_HOST_PREFIX=${SUITE_HOST_PREFIX:-cli-tx-}
# 套件固定名对象（恒删）。
SUITE_VS_FIXED="vs-l2 vs-l3"
SUITE_ACL_FIXED="acl-test"
SUITE_QOS_FIXED="pol-test"
SUITE_VM_FIXED="cli-vm"
SUITE_CT_FIXED="cli-ct2"
SUITE_IMG_FIXED="cli-test.qcow2 cli-del.qcow2"
# 通用名对象（仅当本轮开始前不存在才删）。
SUITE_BOND_AMBIG="bond0"
SUITE_IMG_AMBIG="alpine:3.20"
CLEAN_FAILED=()

_suite_cli() { $CLI -c "$1" 2>&1 | sed '1{/^连接 /d;}'; }

# suite_snapshot_before：套件入口拍「开始前现场」快照（拍完才跑任何阶段）。
suite_snapshot_before() {
  mkdir -p "$SUITE_STATE_DIR"
  _suite_cli "show configuration" > "$SUITE_BEFORE_CFG.tmp" 2>/dev/null
  mv -f "$SUITE_BEFORE_CFG.tmp" "$SUITE_BEFORE_CFG"
  _suite_cli "show images" > "$SUITE_BEFORE_IMG.tmp" 2>/dev/null
  mv -f "$SUITE_BEFORE_IMG.tmp" "$SUITE_BEFORE_IMG"
  printf '· 套件入口已拍开始前现场快照：%s / %s\n' "$SUITE_BEFORE_CFG" "$SUITE_BEFORE_IMG"
}

# _before_has_bond：开始前配置里是否已有该 bond（0=有 1=无 2=快照缺失不可判定）。
_before_has_bond() {
  [ -r "$SUITE_BEFORE_CFG" ] || { echo 2; return; }
  grep -qE "^bonds $1 \{" "$SUITE_BEFORE_CFG" && echo 0 || echo 1
}
_before_has_image() {
  [ -r "$SUITE_BEFORE_IMG" ] || { echo 2; return; }
  grep -qE "^$1([[:space:]]|$)" "$SUITE_BEFORE_IMG" && echo 0 || echo 1
}

# _iface_desc <ifname>：从 committed 配置渲染里取该接口的 description（无则空）。
_iface_desc() {
  printf '%s\n' "$SUITE_CFG" | awk -v n="$1" '
    $0 ~ ("^interfaces " n " \\{") { inb=1; next }
    inb && /^\}/ { inb=0 }
    inb && /^[ \t]*description[ \t]/ { sub(/^[ \t]*description[ \t]*/, ""); sub(/;.*/, ""); print; exit }'
}

# _clean_run：执行一条清场命令；失败（真错误）时记入 CLEAN_FAILED 并打印。
_clean_run() { # _clean_run <描述> <命令>
  local desc="$1"; shift
  local out rc
  out=$(_suite_cli "$*"); rc=$?
  if printf '%s\n' "$out" | _is_fail || [ "$rc" -ne 0 ]; then
    CLEAN_FAILED+=("$desc :: $(printf '%s' "$out" | head -1)")
    printf '  ✗ 清场失败：%s — %s\n' "$desc" "$(printf '%s' "$out" | head -1)"
    return 1
  fi
  return 0
}

# _clean_runtime_config_obj：先尽力停，再删配置条目（幂等：不在配置里就跳过）。
_clean_runtime_config_obj() { # <描述> <配置容器> <名> <停止命令>
  local desc="$1" kind="$2" name="$3" stop="$4"
  printf '%s\n' "$SUITE_CFG" | grep -qE "^$kind $name \{" || return 0
  [ -n "$stop" ] && $CLI -c "$stop" >/dev/null 2>&1   # 停机失败也继续删（删若被校验拦则如实报）
  _clean_run "删除$desc $name" "configure
delete $kind $name
commit"
}

# cleanup_suite：清掉本套件创建的对象，逐条如实报。可安全重复调用（幂等）。
cleanup_suite() {
  local quiet="${1:-}"
  [ "$quiet" = "quiet" ] || { echo; echo "############ 套件收尾清场 ############"; }
  # 连不上服务端/CLI 不可用时**如实说**（不能"没清"却报"已清干净"）——清场本身也需要底座。
  if ! _suite_cli "show version" | grep -q "NFViS"; then
    echo "⚠️ 无法连接 nfvisd 或 CLI（$CLI_BIN）不可用，未做任何清场——请检查 SRV/CLI_BIN 后手动清场"
    return 1
  fi
  SUITE_CFG=$(_suite_cli "show configuration")

  # ① VNF / 容器（先删，避免镜像引用检查挡路）；② 配置对象；③ 镜像；④ 打标字段。
  _clean_runtime_config_obj "VNF" "virtual-machine-functions" "cli-vm" "request virtual-machine-functions cli-vm stop"
  _clean_runtime_config_obj "容器" "container-functions" "cli-ct2" "request container-functions cli-ct2 stop"

  # 配置对象（交换机/ACL/QoS/bond）一次提交：applier 按依赖倒序处理（决策 #196）。
  local del="" o
  for o in $SUITE_VS_FIXED; do printf '%s\n' "$SUITE_CFG" | grep -qE "^virtual-switches $o \{" && del="$del|virtual-switches $o"; done
  for o in $SUITE_ACL_FIXED; do printf '%s\n' "$SUITE_CFG" | grep -qE "^acls $o \{" && del="$del|acls $o"; done
  for o in $SUITE_QOS_FIXED; do printf '%s\n' "$SUITE_CFG" | grep -qE "^qos-policies $o \{" && del="$del|qos policies $o"; done
  # bond0：只有「开始前不存在」（=本轮建的）才删。
  if printf '%s\n' "$SUITE_CFG" | grep -qE "^bonds $SUITE_BOND_AMBIG \{"; then
    case "$(_before_has_bond "$SUITE_BOND_AMBIG")" in
      1) del="$del|bonds $SUITE_BOND_AMBIG";;
      2) printf '  ⊘ 快照缺失，跳过通用名对象 bonds %s（不误删用户既有对象）\n' "$SUITE_BOND_AMBIG";;
      *) printf '  ⊘ bonds %s 在套件开始前已存在，保留（非本轮创建）\n' "$SUITE_BOND_AMBIG";;
    esac
  fi
  if [ -n "$del" ]; then
    _clean_run "删除配置对象（交换机/ACL/QoS/bond）" "configure
$(printf '%s\n' "$del" | tr '|' '\n' | grep -v '^$' | sed 's/^/delete /')
commit"
  fi

  # 接口描述：仅清 == cli-pre 的（按值识别，不碰用户自设描述）。
  local ifd
  for ifd in ens192 ens224; do
    if [ "$(_iface_desc "$ifd")" = "$SUITE_DESC_MARK" ]; then
      _clean_run "清除接口 $ifd 的套件描述" "configure
delete interfaces $ifd description
commit"
    fi
  done

  # 镜像：固定名恒删（幂等）；alpine:3.20 仅当本轮导入（开始前不存在）才删。
  local img all_imgs
  all_imgs=$(_suite_cli "show images")
  for img in $SUITE_IMG_FIXED; do
    if printf '%s\n' "$all_imgs" | grep -qE "^$img([[:space:]]|$)"; then
      _clean_run "删除镜像 $img" "request images delete name $img --yes"
    fi
  done
  if printf '%s\n' "$all_imgs" | grep -qE "^$SUITE_IMG_AMBIG([[:space:]]|$)"; then
    case "$(_before_has_image "$SUITE_IMG_AMBIG")" in
      1) _clean_run "删除镜像 $SUITE_IMG_AMBIG" "request images delete name $SUITE_IMG_AMBIG --yes";;
      2) printf '  ⊘ 快照缺失，跳过通用名镜像 %s（不误删用户既有对象）\n' "$SUITE_IMG_AMBIG";;
      *) printf '  ⊘ 镜像 %s 在套件开始前已存在，保留（非本轮导入）\n' "$SUITE_IMG_AMBIG";;
    esac
  fi

  # 主机名：仅当被套件改成 cli-tx-* 才还原（阶段 6 的提交值；按值识别，不碰用户主机名）。
  local host
  host=$(_suite_cli "show configuration" | sed -nE 's/^[[:space:]]*hostname[[:space:]]+([^;]+);.*/\1/p' | head -1 | tr -d '\r')
  case "$host" in
    "$SUITE_HOST_PREFIX"*)
      _clean_run "还原套件写入的主机名（$host）" "configure
delete system hostname
commit" ;;
  esac

  # 临时文件（床铺级清理，不属"对象"）。
  rm -f /data/incoming/cli-test.qcow2 /data/incoming/cli-del.qcow2 /data/incoming/ct.tar /tmp/cli-candidate.json 2>/dev/null

  if [ "${#CLEAN_FAILED[@]}" -eq 0 ]; then
    [ "$quiet" = "quiet" ] || echo "· 套件对象已清干净（未清项 0）"
    return 0
  fi
  echo "⚠️ 有 ${#CLEAN_FAILED[@]} 项未清掉（如实登记，不静默）："
  local f; for f in "${CLEAN_FAILED[@]}"; do echo "  - $f"; done
  return 1
}
