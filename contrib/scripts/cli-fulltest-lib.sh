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

# expect_ping_coherent：ping 的**判定自洽**（附录 A #89）。
#
# 不变量（这条才是修复的真正价值）：**0 发包绝不能被算作通过**。0 发包时必须出现 %% 错误；
# 若 0 发包却判通过，就是修复前那种假绿（真机实测：修复前把 0 发包的 ping 算作 ✓）。
# 真发过包时属于环境相关结果（通不通取决于 VPP 有没有到该目标的路由），不再额外要求。
#
# 为什么不由脚本「预测」环境（例如看 VPP 有没有 L3 地址就决定 run 还是 expect_fail）：
# 实测同一环境里 `ping <host>`（走 FIB，无路由即 0 发包）与
# `ping <host> source <iface>`（显式出接口，照样发得出去）**结论可以不同**，
# 预测式分流必然误判；而本函数只断言「结果与判定自洽」，与环境无关。
expect_ping_coherent() { # expect_ping_coherent <阶段> <命令>
  local phase="$1"; shift
  local cmd="$*" out rc sent0=0
  out=$($CLI -c "$cmd" 2>&1); rc=$?
  out=$(printf '%s\n' "$out" | sed '1{/^连接 /d;}')
  printf '%s\n' "$out" | grep -qE 'Statistics:[[:space:]]*0 sent' && sent0=1
  if [ "$sent0" = 1 ]; then
    if printf '%s\n' "$out" | _is_fail || [ "$rc" -ne 0 ]; then
      EXPECTED=$((EXPECTED+1)); printf '  ⊘ %s（0 发包 → 正确报失败）\n' "$cmd"
    else
      FAIL=$((FAIL+1))
      FAILED_LIST+=("[$phase] $cmd :: 0 发包却未报失败（假绿回归）")
      printf '  ✗ %s（0 发包却算通过）\n' "$cmd"
      printf '%s\n' "$out" | sed 's/^/       | /' | head -5
    fi
  elif printf '%s\n' "$out" | _is_fail || [ "$rc" -ne 0 ]; then
    FAIL=$((FAIL+1))
    FAILED_LIST+=("[$phase] $cmd :: 真发出包却报错")
    printf '  ✗ %s\n' "$cmd"
    printf '%s\n' "$out" | sed 's/^/       | /' | head -5
  else
    PASS=$((PASS+1)); printf '  ✓ %s（真发包）\n' "$cmd"
  fi
  printf '=== [%s][ping-coherence] %s\n%s\n\n' "$phase" "$cmd" "$out" >> "$LOG"
}
