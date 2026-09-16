#!/usr/bin/env bash
# 守护：internal/api/openapi.json（编译期嵌入二进制、经 GET /api/v1/openapi.json 发布的
# 运行时规范副本）必须与契约真源 docs/NFViS-openapi.yaml 一致（FR-API-002，决策 #69）。
# 背景：规范既要随包发布、又要运行时可取，故在二进制中嵌入一份；
# 双份内容若无守护必然漂移，故由本脚本自动校验。
set -e
cd "$(git rev-parse --show-toplevel)"

SRC=docs/NFViS-openapi.yaml
DST=internal/api/openapi.json

# python3 为 Linux/CI 惯例，Windows 开发机（Git Bash）通常只有 python
PY=$(command -v python3 || command -v python || true)
if [ -z "$PY" ]; then
    echo "✗ 需要 python3/python（含 PyYAML）"
    exit 1
fi

gen() {
    "$PY" - "$SRC" <<'PY'
import json, sys, yaml
# 强制 LF 与 UTF-8：仓库 .gitattributes 规定 eol=lf，而 Windows 的 Python 默认
# 把 \n 翻译为 \r\n、stdout 跟随 GBK 控制台编码——规范描述含 ⇄ 等非 GBK 字符，
# 任一不符都会让本守护在 Windows 开发机上崩溃或恒报「不同步」。
sys.stdout.reconfigure(newline="\n", encoding="utf-8")
spec = yaml.safe_load(open(sys.argv[1], encoding='utf-8'))
# sort_keys 保证输出确定性，便于 diff 守护
print(json.dumps(spec, ensure_ascii=False, indent=2, sort_keys=True))
PY
}

if [ "$1" = "--write" ]; then
    gen >"$DST"
    echo "✓ 已由 $SRC 生成 $DST"
    exit 0
fi

if [ ! -f "$DST" ]; then
    echo "✗ 缺少 $DST——请运行：bash contrib/scripts/check_openapi_json_sync.sh --write"
    exit 1
fi

if ! diff -q <(gen) "$DST" >/dev/null; then
    echo "✗ $DST 与 $SRC 不同步（契约先行，AGENTS.md 规则 1）"
    echo "  请运行：bash contrib/scripts/check_openapi_json_sync.sh --write"
    exit 1
fi
echo "✓ openapi.json 与契约 YAML 同步"
