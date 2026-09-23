#!/usr/bin/env bash
# 控制台配置页「口令就地派生」路径的桩式自校准（壳 + 红-绿验证；DOM 桩见同名 .js）。
#
# 由来（2026-09-24，round61 证据 §5 的判定）：明文 HTTP 打开控制台时，口令控件填明文 →
# 点「保存到 candidate」→ 界面报「已保存到 candidate（有未提交变更）」，而独立事实源
# GET /configuration/candidate 里该用户的 password_hash 是空的——**口令改动被静默丢掉、
# 界面却报成功**（假绿）。同一份证据还记下了为什么上一轮的桩没抓到：那个桩跑在 Node 里
# （有 crypto.subtle，等价"安全上下文"），**验证环境比目标环境更强**，这条路径根本没走到。
#
# 故本脚本跑两件事：
#   ① 同名 .js 的桩式自校准（屏蔽 / 提供 crypto.subtle 各跑一遍真实表单路径）；
#   ② 红-绿：把「保存前的口令闸门」拆掉一行，同一套用例**必须**报 ✗——
#      否则这些断言只是恒绿，挡不住回归（本仓库对"守卫要验证它真的生效"的一贯要求）。
#
# 用法：bash contrib/scripts/web-console-config-selftest.sh
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
STUB=$HERE/web-console-config-selftest.js
APP=$HERE/../../internal/api/ui/app.js

# node 只在跑本项时需要（前端与产品构建都不依赖它）：没有就如实说"没跑"，别让人误当成通过。
if ! command -v node >/dev/null 2>&1; then
  echo "跳过控制台口令路径自校准：未找到 node（本项**没有跑**；本仓库其余检查不依赖 node）"
  exit 0
fi

RC=0
node "$STUB" || RC=1

# 红-绿：把闸门调用点换成 `true`（= 保存前不再检查口令有没有派生出哈希）。
TMPD=$(mktemp -d) || exit 1
trap 'rm -rf "$TMPD"' EXIT
sed 's/await cfgPwGate()/true/' "$APP" > "$TMPD/app.js"
BEFORE=$(grep -c 'await cfgPwGate()' "$APP")
AFTER=$(grep -c 'await cfgPwGate()' "$TMPD/app.js")
if [ "$BEFORE" != "1" ] || [ "$AFTER" != "0" ]; then
  echo "✗ 红-绿用例没能改到闸门调用点（锚点变了？原文件命中 $BEFORE 处）——这条自校准本身要修"
  RC=1
elif WEB_CONSOLE_APP_JS="$TMPD/app.js" node "$STUB" >/dev/null 2>&1; then
  echo "✗ 拆掉保存前的口令闸门后仍然全绿——断言没有判别力（这正是假绿的成因）"
  RC=1
else
  echo "✓ 拆掉保存前的口令闸门后同一套用例报 ✗（断言确实在判别，不是恒绿）"
fi

if [ $RC -eq 0 ]; then echo "全部符合预期"; else echo "有不符合预期的用例"; fi
exit $RC
