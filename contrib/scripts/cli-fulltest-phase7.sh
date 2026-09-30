#!/usr/bin/env bash
# 阶段 7：CLI **脚本文件模式** `-f <file>`（决策 #309）。
#
# 为什么自成一阶段：`-f` 是**客户端**开关（不是服务端命令，故不在《命令全表》里，
# 也不由 contrib/scripts/check_suite_contract_sync.sh 守护——那条守护只管契约命令；
# 本阶段是手写的对应覆盖，决策 #319 已如实登记）。判定口径也与阶段 1~6 的 run 不同：
# 量的是「按行执行 / 真错误即停 / 行尾归一 / 互斥与缺文件报错」这些客户端行为。
# 与 pty 冒烟（cli-pty-smoke.sh）的分工：pty 量按键与行编辑器，这里量**批处理来源**。
#
# 判据：与 run 同源——行首 % / %% 或退出码非 0 即失败；另附必要的关键词断言
# （失败要说出原因与路径，不能只回一个非零码）。
set -u
source "$(cd "$(dirname "$0")" && pwd)/cli-fulltest-lib.sh"

echo "############ 阶段 7：CLI 脚本文件模式（-f） ############"
{ echo "############ 阶段 7：CLI 脚本文件模式（-f） ############"; } >> "$LOG"

WORK=${WORK:-/tmp/cli-test/phase7}
mkdir -p "$WORK"

# 客户端开关直调（不套 -c）：`-f` 与 `-c` 互斥，故不能经 run()。
fcli() { "$CLI_BIN" -server "$SRV" -u admin "$@" 2>&1; }

# judge：<标签> <期望 pass|fail> <必含关键词（无则 -）> <输出> <退出码>
judge() {
  local label="$1" want="$2" kw="$3" out="$4" rc="$5" got=pass
  printf '%s\n' "$out" | _is_fail && got=fail
  [ "$rc" -ne 0 ] && got=fail
  local miss=0
  if [ "$kw" != "-" ] && ! printf '%s\n' "$out" | grep -qF -- "$kw"; then miss=1; fi
  if [ "$got" = "$want" ] && [ "$miss" = 0 ]; then
    PASS=$((PASS+1)); printf '  ✓ %s（%s%s）\n' "$label" "$got" "${kw:+，含「$kw」}"
  else
    FAIL=$((FAIL+1))
    FAILED_LIST+=("[$label] 期望 $want$([ "$kw" != "-" ] && printf '且含「%s」' "$kw")，实际 $got$([ "$miss" = 1 ] && printf '（缺关键词）')")
    printf '  ✗ %s（期望 %s%s，实际 %s）\n' "$label" "$want" "${kw:+ 含「$kw」}" "$got"
    printf '%s\n' "$out" | sed 's/^/       | /' | head -6
  fi
  printf '=== [S7][%s]\n%s\n\n' "$label" "$out" >> "$LOG"
}

# ---- 7-1 正常脚本：逐行执行、退出 0 ----
cat > "$WORK/ok.txt" <<'EOF'
show version
show system api tokens
EOF
out=$(fcli -f "$WORK/ok.txt"); rc=$?
judge "7-1 -f 正常脚本（两行 show）逐行执行" pass "NFViS" "$out" "$rc"

# ---- 7-2 真错误行即停：其后语句不得执行（用第二个未知子命令的特征串反证）----
cat > "$WORK/stop.txt" <<'EOF'
show version
show system no-such-subcommand-xyz
show vpp also-no-such-subcommand-abc
EOF
out=$(fcli -f "$WORK/stop.txt"); rc=$?
judge "7-2 真错误行即停（退出码非 0）" fail "no-such-subcommand-xyz" "$out" "$rc"
if printf '%s\n' "$out" | grep -qF 'also-no-such-subcommand-abc'; then
  FAIL=$((FAIL+1))
  FAILED_LIST+=("[7-2] 错误行之后的语句被执行了（未即停）")
  printf '  ✗ 7-2 错误行之后的语句被执行了（未即停）\n'
else
  PASS=$((PASS+1)); printf '  ✓ 7-2 错误行之后的语句未执行（确实即停）\n'
fi
printf '=== [S7][7-2-stop-proof]\n%s\n\n' "$out" >> "$LOG"

# ---- 7-3 CRLF 行尾（Windows 常见）：归一后照常执行，不因 commit\r 一类报语法错 ----
printf 'show version\r\nshow system api tokens\r\n' > "$WORK/crlf.txt"
out=$(fcli -f "$WORK/crlf.txt"); rc=$?
judge "7-3 CRLF 行尾文件照常执行" pass "NFViS" "$out" "$rc"

# ---- 7-4 缺文件：明确报错并含路径 ----
out=$(fcli -f "$WORK/no-such-file.txt"); rc=$?
judge "7-4 缺文件报错含路径" fail "no-such-file.txt" "$out" "$rc"

# ---- 7-5 -c 与 -f 互斥：同时给出必须报错（不许静默取其一）----
out=$(fcli -c "show version" -f "$WORK/ok.txt"); rc=$?
judge "7-5 -c 与 -f 互斥" fail "只能选其一" "$out" "$rc"

# ---- 7-6 -f - 读 stdin（口令须由 -p/NFVIS_PASSWORD 提供）----
out=$(printf 'show version\n' | "$CLI_BIN" -server "$SRV" -u admin -p "$NFVIS_PASSWORD" -f - 2>&1); rc=$?
judge "7-6 -f - 从 stdin 读脚本" pass "NFViS" "$out" "$rc"

summary "阶段 7"
