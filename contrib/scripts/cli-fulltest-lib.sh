#!/usr/bin/env bash
# 共用 harness：CLI 全功能测试。
# 失败判定：行首出现 % / %% 错误，或出现 "校验失败"。
# 说明：CLI 脚本模式(%%)会中止；配置模式的行内错误是单 %，须显式匹配行首 %。
SRV="http://127.0.0.1:18443"
CLI="/tmp/nfvis-cli -server $SRV -u admin"
export NFVIS_PASSWORD="Admin@12345"
LOG=${LOG:-/tmp/cli-test/full.log}
mkdir -p /tmp/cli-test

PASS=0; FAIL=0; FAILED_LIST=()

_is_fail() { # stdin: 输出；返回 0 表示失败
  grep -qE '^%%|^%[^%]|^%$|校验失败|^%% ' 
}

run() { # run <阶段id> <命令>
  local phase="$1"; shift
  local cmd="$*" out rc
  out=$($CLI -c "$cmd" 2>&1); rc=$?
  out=$(printf '%s\n' "$out" | sed '1{/^连接 /d;}')
  if printf '%s\n' "$out" | _is_fail || [ $rc -ne 0 ]; then
    FAIL=$((FAIL+1))
    local brief; brief=$(printf '%s\n' "$out" | grep -nE '^%%|^%[^%]|^%$|校验失败' | head -2 | tr '\n' ' ')
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
