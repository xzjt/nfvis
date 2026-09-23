#!/usr/bin/env bash
# 校验 AGENTS.md 声明的决策条数与规格书附录 A 实际条数一致 + 附录 A 表格结构完整。
# 背景：① 条数多次滞后于附录 A（24→28→34 三次），人工同步不可靠，故自动守护；
#       ② 2026-09-23 发现附录 A 的 #130 行**被劈成两半**——行内写的是**真 CR 字符**（作者当时把
#          字面 `\r` 的转义写坏了），后来某次编辑把该 CR 规范成换行：行尾断在「（补 `」，
#          后半截（`）、「只发回车」…）掉到表尾成了孤立行，**条数守护照样数得对**（红线 4/15
#          同一类事故，只是这次伤在文档结构上）。故补两道结构检查：
#          · 每条决策行必须以 `|` 或 `。` 收尾（截断行的行尾是半个代码段/逗号一类）；
#          · 首尾决策行之间不允许出现「非空、又不以 | 开头」的行（劈开的尾巴就是这种行）。
#       注：本脚本是**开发/CI 工具**，不是操作者读物，但按 .sh 的统一口径，非注释行里也不写内部引用。
set -e
cd "$(git rev-parse --show-toplevel)"

SPEC=docs/NFViS-系统产品需求与目标架构规格书.md
declared=$(grep -oE '已定决策 [0-9]+ 项' AGENTS.md | grep -oE '[0-9]+' | head -1)
actual=$(grep -cE '^\| [0-9]+ \|' "$SPEC")

if [ -z "$declared" ]; then
    echo "✗ AGENTS.md 中未找到决策条数声明"
    exit 1
fi
if [ "$declared" != "$actual" ]; then
    echo "✗ AGENTS.md 声明 $declared 项，规格书实际 $actual 项——请同步 AGENTS.md「项目状态与基线」"
    exit 1
fi

# 结构检查 1：决策行必须完整收尾（`|` 或句号）
truncated=$(grep -nE '^\| [0-9]+ \|' "$SPEC" | grep -vE '(\||。)$' || true)
if [ -n "$truncated" ]; then
    echo "✗ 决策记录里有行的行尾被截断（应止于 '|' 或 '。'）："
    echo "$truncated" | cut -c1-120 | sed 's/^/    /'
    exit 1
fi

# 结构检查 2：首尾决策行之间不得有孤立行（劈开的尾巴）
first=$(grep -nE '^\| [0-9]+ \|' "$SPEC" | head -1 | cut -d: -f1)
last=$(grep -nE '^\| [0-9]+ \|' "$SPEC" | tail -1 | cut -d: -f1)
stray=$(awk -v a="$first" -v b="$last" 'NR>a && NR<b && NF>0 && substr($0,1,1)!="|"' "$SPEC" || true)
if [ -n "$stray" ]; then
    echo "✗ 决策记录区间里有不以 '|' 开头的孤立行（疑似被劈开的行尾）："
    echo "$stray" | cut -c1-120 | sed 's/^/    /'
    exit 1
fi

echo "✓ 决策条数一致（$actual 项）且决策记录表格结构完整"
