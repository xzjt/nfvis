#!/usr/bin/env bash
# 控制台「分级确认」的桩式自校准（壳 + 红-绿验证；DOM 桩见同名 .js）。
#
# 这一项要回答的问题（本仓库对"守卫要验证它真的生效"的一贯要求）：
#   ① 高危档的两道闸门**真的拦得住**吗——确认词不匹配、倒计时没走完时，
#      「执行」按钮是不是真的点不动（不是"提示了一句"就算数）；倒计时用**假时钟**推进，
#      故能确定地断言"差 1 秒也不能点"（真等待只会变成碰运气）；
#   ② 界面上每个写操作**归到哪一档**是不是真的按表来的——中危必须列出影响面并标红主按钮，
#      低危不该被一屏字挡住，高危必须有确认词与倒计时；改档位要让本项报 ✗；
#   ③ 确认之前与取消之后**一条写请求都不许发**（桩 fetch 记录就是事实源；只读预检不算写）。
#
# 故本脚本跑三件事：
#   ① 同名 .js 的桩式自校准（真实 app.js + 最小 DOM 桩，逐个动作走一遍）；
#   ② 红-绿：把闸门或档位拆掉（见 .js 里的变异表），同一套用例**必须**报 ✗——
#      否则这些断言只是恒绿，挡不住回归。
#   ③ 换一个浏览器时区把整份用例复跑一遍：记录类时间按 UTC 渲染（round80 缺陷），
#      就不该随浏览器时区变——这一条**只能在真时区里跑**才说明问题（断言本身按 UTC 字面量比对，
#      但"本机是 UTC 时碰巧一致"不算验证过；见 .js 的 ⑩）。
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

# ③ 跨时区复跑：先自校准"本机 node 认哪些 TZ 名"（Windows 上的 node 不认 IANA 名、
# 只认 POSIX 形式如 EST5EDT；Linux 上两者都认），认不出就如实说明，不假装扫过时区。
BASE_OFF=$(node -e 'process.stdout.write(String(new Date("2026-09-25T01:24:22Z").getTimezoneOffset()))')
TZSEL=""
TZOFF=""
for z in EST5EDT America/New_York Asia/Tokyo GMT-3; do
  off=$(TZ="$z" node -e 'process.stdout.write(String(new Date("2026-09-25T01:24:22Z").getTimezoneOffset()))' 2>/dev/null || true)
  if [ -n "$off" ] && [ "$off" != "$BASE_OFF" ]; then TZSEL=$z; TZOFF=$off; break; fi
done
if [ -n "$TZSEL" ]; then
  if [ "$BASE_OFF" -ge "$TZOFF" ]; then TZDIFF=$(( (BASE_OFF - TZOFF) / 60 )); else TZDIFF=$(( (TZOFF - BASE_OFF) / 60 )); fi
  if TZ="$TZSEL" node "$STUB" >"$TMPD/tz.out" 2>&1; then
    echo "✓ TZ=$TZSEL（与本机默认时区相差 $TZDIFF 小时）下同一套用例仍全绿：记录类时间不随浏览器时区变"
  else
    echo "✗ TZ=$TZSEL 下报 ✗：$(grep -m1 '✗' "$TMPD/tz.out" | sed 's/^ *//')"
    RC=1
  fi
else
  echo "（本机 node 不认候选 TZ 名，未做跨时区复跑；时间口径的断言按 UTC 字面量比对，本身与时区无关）"
fi

MUT=$TMPD/app.js
for m in gates word timer reboot zeroize dpdk rawhttp acctgate utctime; do
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
