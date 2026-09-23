#!/usr/bin/env bash
# 控制台「分级确认」的桩式自校准（壳 + 红-绿验证；DOM 桩见同名 .js）。
#
# 这一项要回答的问题（本仓库对"守卫要验证它真的生效"的一贯要求）：
#   ① 高危档的两道闸门**真的拦得住**吗——确认词不匹配、倒计时没走完时，
#      「执行」按钮是不是真的点不动（不是"提示了一句"就算数）；
#   ② 界面上每个写操作**归到哪一档**是不是真的按表来的——中危必须列出影响面并标红主按钮，
#      低危不该被一屏字挡住；改档位要让本项报 ✗；
#   ③ 确认之前与取消之后**一条请求都不许发**（桩 fetch 记录就是事实源）。
#
# 故本脚本跑两件事：
#   ① 同名 .js 的桩式自校准（真实 app.js + 最小 DOM 桩，逐个动作走一遍）；
#   ② 红-绿：把闸门或档位拆掉（见 .js 里的变异表），同一套用例**必须**报 ✗——
#      否则这些断言只是恒绿，挡不住回归。
#
# 用法：bash contrib/scripts/web-console-confirm-selftest.sh
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
STUB=$HERE/web-console-confirm-selftest.js

# node 只在跑本项时需要（前端与产品构建都不依赖它）：没有就如实说"没跑"，别让人误当成通过。
if ! command -v node >/dev/null 2>&1; then
  echo "跳过控制台分级确认自校准：未找到 node（本项没有跑；本仓库其余检查不依赖 node）"
  exit 0
fi

RC=0
node "$STUB" || RC=1

TMPD=$(mktemp -d) || exit 1
trap 'rm -rf "$TMPD"' EXIT
MUT=$TMPD/app.js
for m in gates word timer reboot; do
  if ! node "$STUB" --mutate "$m" "$MUT"; then
    echo "✗ 生成变异体 $m 失败（锚点对不上）——本自校准自身要修"
    RC=1
    continue
  fi
  if WEB_CONSOLE_APP_JS="$MUT" node "$STUB" >"$TMPD/$m.out" 2>&1; then
    echo "✗ 拆掉 $m 之后仍然全绿——断言没有判别力（这正是假绿的成因）"
    RC=1
  else
    echo "✓ 拆掉 $m 之后同一套用例报 ✗：$(grep -m1 '✗' "$TMPD/$m.out" | sed 's/^ *//')"
  fi
done

if [ $RC -eq 0 ]; then echo "全部符合预期"; else echo "有不符合预期的用例"; fi
exit $RC
