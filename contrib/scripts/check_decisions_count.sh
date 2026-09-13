#!/usr/bin/env bash
# 校验 AGENTS.md 声明的决策条数与规格书附录 A 实际条数一致。
# 背景：该数字多次滞后于附录 A（24→28→34 三次），人工同步不可靠，故自动守护。
set -e
cd "$(git rev-parse --show-toplevel)"

SPEC=docs/NFViS-系统产品需求与目标架构规格书.md
declared=$(grep -oE '已定决策 [0-9]+ 项' AGENTS.md | grep -oE '[0-9]+' | head -1)
actual=$(grep -cE '^\| [0-9]+ \|' "$SPEC")

if [ -z "$declared" ]; then
    echo "✗ AGENTS.md 中未找到「已定决策 N 项」声明"
    exit 1
fi
if [ "$declared" != "$actual" ]; then
    echo "✗ AGENTS.md 声明 $declared 项，附录 A 实际 $actual 项——请同步 AGENTS.md「项目状态与基线」"
    exit 1
fi
echo "✓ 决策条数一致（$actual 项）"
