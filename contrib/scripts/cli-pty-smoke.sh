#!/usr/bin/env bash
# NFViS CLI 交互（pty）冒烟测试（**手动**，不在 CI / make check 中运行）
#
# 用途：验证**行编辑器级**的交互行为。这些都是非 TTY（管道/脚本）下会被完全掩盖、
# 必须给子进程一个 pty 才能复现的行为：
#   1) raw 模式下输出换行正确（候选列表/提示符不得阶梯错位）；
#   2) Tab 多匹配「响铃并列出」候选（FR-CLI-003 / 命令树设计 §5.2）；
#   3) `?` 按键即时列候选、按前缀过滤、且不进入行文本（FR-CLI-002 / §5.1）；
#   4) 列宽按最长候选计算——长 token 不得与描述粘连（§5.6）；
#   5) 非 TTY 路径不得被 CRLF 污染（管道输出仍应为 LF，§5.7）。
#
# 由来：2026-09-15 用户真机反馈暴露 1)~4)（决策 #81）。此前的验收与 M2 演示是用
#       `printf … | nfvis-cli`（非 TTY）跑的：MakeRaw 失败退化为 readPlain，于是
#       「错位」与「需回车」都不出现，FR-CLI-001/002/003 被误判为「通过」。
#       **结论：交互行为必须在 pty 下验证，管道演示不能充当交互证据。**
#
# 前置：
#   1) nfvisd 可连（开发态建议 127.0.0.1:18443 + -allow-plaintext）
#   2) util-linux 的 script(1)（提供 pty）；缺失则跳过（不误报失败）
#
# 用法：
#   bash contrib/scripts/cli-pty-smoke.sh [CLI 路径] [server] [user] [password]
#   缺省：/usr/bin/nfvis-cli  https://127.0.0.1:443  admin  $NFVIS_PASSWORD
set -u

CLI=${1:-/usr/bin/nfvis-cli}
SRV=${2:-https://127.0.0.1:443}
CLI_USER=${3:-admin}
CLI_PW=${4:-${NFVIS_PASSWORD:-}}
LOGIN_DELAY=${NFVIS_PTY_LOGIN_DELAY:-4} # 等登录完成的秒数（慢环境可调大）

if ! command -v script >/dev/null 2>&1; then
  echo "跳过 cli-pty-smoke：未找到 script(1)（util-linux）——pty 冒烟需要它"
  exit 0
fi
if [ ! -x "$CLI" ]; then
  echo "✗ CLI 不可执行: $CLI"
  exit 1
fi
if [ -z "$CLI_PW" ]; then
  echo "✗ 未提供口令：用第 4 个参数或 NFVIS_PASSWORD 指定"
  exit 1
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

PASS=0
FAIL=0
ok()  { echo "  ✓ $1"; PASS=$((PASS + 1)); }
bad() { echo "  ✗ $1"; FAIL=$((FAIL + 1)); }

# run_pty <按键序列> <输出文件>：登录完成后才注入按键（避开 cooked 模式的回声干扰），
# 随后 Ctrl-C 清行、Ctrl-D 退出。
run_pty() {
  local keys="$1" out="$2"
  {
    sleep "$LOGIN_DELAY"
    printf '%b' "$keys"
    sleep 3
    printf '\003'
    sleep 1
    printf '\004'
    sleep 2
  } | timeout 60 script -qec "$CLI -server $SRV -u $CLI_USER -p $CLI_PW" /dev/null >"$out" 2>&1
}

# 候选行计数（行首两空格 + 小写字母开头）
count_cand() { grep -c '^  [a-z]' "$1" 2>/dev/null || true; }

echo "pty 冒烟：CLI=$CLI server=$SRV user=$CLI_USER"
echo

echo "[1] \`show ?\`（**不发回车**）：按键即时列候选 + 候选行 CRLF + 长 token 不粘连"
run_pty 'show ?' "$TMP/1.raw"
cand_total=$(count_cand "$TMP/1.raw")
if [ "$cand_total" -ge 2 ]; then
  ok "候选已列出（$cand_total 条）"
else
  bad "未列出候选——\`?\` 未按键生效？"
fi
# 只统计候选行的行尾：全量统计会在「一条候选都没有」时假通过
cand_crlf=$(grep '^  [a-z]' "$TMP/1.raw" 2>/dev/null | grep -c $'\r$' || true)
if [ "$cand_total" -eq 0 ]; then
  bad "无候选行可判定行尾（候选未列出，见上一条）"
elif [ "$cand_crlf" -eq "$cand_total" ]; then
  ok "候选行全部以 CRLF 结尾（裸 LF 会在 raw 下阶梯错位）"
else
  bad "有 $((cand_total - cand_crlf)) 条候选行是裸 LF ——raw 模式下会逐行右移（阶梯错位）"
fi
if grep -qE 'virtual-machine-functions[[:space:]]+[^[:space:]]' "$TMP/1.raw"; then
  ok "长 token（25 字符）与描述未粘连"
else
  bad "长 token 与描述粘连——列宽仍按固定 24？"
fi
if grep -q 'show ?' "$TMP/1.raw"; then
  bad "\`?\` 进入了行文本（帮助键不应回显）"
else
  ok "\`?\` 未进入行文本"
fi

echo
echo "[2] \`show \` + Tab：多匹配应响铃**并**列出候选"
run_pty 'show \t' "$TMP/2.raw"
if grep -q $'\a' "$TMP/2.raw"; then ok "已响铃"; else bad "未响铃"; fi
if [ "$(count_cand "$TMP/2.raw")" -ge 2 ]; then
  ok "已列出候选（$(count_cand "$TMP/2.raw") 条）"
else
  bad "未列出候选——Tab 多匹配只响铃？"
fi

echo
echo "[3] \`show ver?\`（**不发回车**）：按前缀过滤并即时列出"
run_pty 'show ver?' "$TMP/3.raw"
if grep -qE '^  version[[:space:]]+' "$TMP/3.raw"; then
  ok "已即时列出匹配前缀的 version"
else
  bad "未列出候选——\`?\` 仍需回车才生效？"
fi
if grep -q 'show ver?' "$TMP/3.raw"; then
  bad "\`?\` 进入了行文本"
else
  ok "\`?\` 未进入行文本"
fi

echo
echo "[4] 非 TTY（管道）：不得被 CRLF 污染，且仍应列候选"
printf 'show ?\n' | timeout 60 "$CLI" -server "$SRV" -u "$CLI_USER" -p "$CLI_PW" >"$TMP/4.raw" 2>&1
if grep -q $'\r' "$TMP/4.raw"; then
  bad "管道输出出现 CR——非 raw 路径不应做换行转换"
else
  ok "管道输出无 CR（仍为 LF）"
fi
if [ "$(count_cand "$TMP/4.raw")" -ge 2 ]; then
  ok "非 TTY 路径仍列出候选"
else
  bad "非 TTY 路径未列候选（退化路径回归）"
fi

echo
echo "==================== 结果：通过 $PASS / 失败 $FAIL ===================="
if [ "$FAIL" -gt 0 ]; then
  echo "失败详情可参考本次输出；docs/evidence/v1-closeout-round12.txt 有前后对照。"
  exit 1
fi
echo "全部通过。"
