#!/usr/bin/env bash
# 守护：契约 YAML **不得有重复的映射键**（决策 #116）。
#
# 由来（R37-1，2026-09-22）：`components/schemas/VppStatus` 在 docs/NFViS-openapi.yaml 里
# 被定义了**两次**（一处与实现一致、一处过时）。而 PyYAML 的 `safe_load` 对重复键**静默保留
# 最后一个**，于是 `check_openapi_json_sync.sh` 生成的内嵌 openapi.json 带的是**过时形状**——
# 发布出去的规范与实现不符，照它开发的 Web 控制面取错字段，而 `routes_contract_test.go`
# 只守护「路由 ⊆ 契约」（路径），既看不见重复键、也不看响应形状。
#
# 本脚本用**真正的解析器**逐层查重（不是正则扫行——契约里有 `{name}:start` 这类含冒号的路径、
# 有内联 map、有列表项，按行扫必出假阳性；实测正则版会误报 62 处，真重复只有 1 处）。
set -e
cd "$(git rev-parse --show-toplevel)"

SRC=docs/NFViS-openapi.yaml
PY=$(command -v python3 || command -v python || true)
if [ -z "$PY" ]; then
    echo "✗ 需要 python3/python（含 PyYAML）"
    exit 1
fi

"$PY" - "$SRC" <<'PY'
import sys
import yaml

path = sys.argv[1]


class DuplicateCheckingLoader(yaml.SafeLoader):
    """SafeLoader + 逐 mapping 查重（YAML 规范不允许重复键，但 PyYAML 默认静默取最后一个）。"""


def construct_mapping(loader, node, deep=False):
    seen = {}
    for key_node, _ in node.value:
        key = loader.construct_object(key_node, deep=deep)
        try:
            hash(key)
        except TypeError:      # 不可哈希的键交给上游报错
            break
        if key in seen:
            raise yaml.constructor.ConstructorError(
                None, None,
                "重复键 %r：第 %d 行 与 第 %d 行"
                % (key, seen[key] + 1, key_node.start_mark.line + 1),
                key_node.start_mark,
            )
        seen[key] = key_node.start_mark.line
    return yaml.SafeLoader.construct_mapping(loader, node, deep)


DuplicateCheckingLoader.add_constructor(
    yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, construct_mapping
)

try:
    with open(path, encoding="utf-8") as fh:
        yaml.load(fh, Loader=DuplicateCheckingLoader)
except yaml.constructor.ConstructorError as e:
    print("✗ %s 存在重复键（PyYAML 会静默保留最后一个，发布的规范将与你看到的不一致）" % path)
    print("  %s" % e.problem)
    sys.exit(1)
print("✓ 契约 YAML 无重复键")
PY
