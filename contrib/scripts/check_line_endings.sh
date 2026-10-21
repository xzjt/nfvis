#!/usr/bin/env bash
# 守护：仓库内的文本文件不得含 CRLF（`.gitattributes` 已声明 `* text=auto eol=lf`）。
#
# 由来（round41 决策 #495）：子智能体在 Windows 上改 `docs/NFViS-openapi.yaml` 时把**整个文件**
#   的行尾换成了 CRLF。git 因 `.gitattributes` 的 `eol=lf` 会在提交时归一化，`git diff` 只显示
#   真实的 30 行改动——**污染在 git 层面看不见**；但工作区里的文件是 CRLF，而多个契约守护是
#   **按 `\n` 切行后做精确字符串比较**（如 `if l == "    SwitchPortView:"`）。CRLF 下每行尾部
#   多一个 `\r`，精确匹配永远不命中 ⇒ `TestUIQosCardFieldsExistInContract` /
#   `TestUISpanCardFieldsExistInContract` / `TestUISwitchPortTableFieldsExistInContract` 三条
#   守护同时报「契约里找不到 schema（排版变了？）」，**看起来像契约排版坏了，实际是行尾**。
#   更糟的是 `make check` 在 CI（Linux 检出为 LF）上会全绿，只在 Windows 工作区里红。
#
# 判据：被 git 跟踪的文本文件（按 .gitattributes 的 text 归类，排除 binary）中不得出现 CR。
#   `--eol` 的 `w/crlf` 只报**工作区**是 CRLF 的文件；索引里是 LF 时仍能抓到工作区污染。
# 用法：check_line_endings.sh            # 检查全仓（缺省，供 make check / toolcheck 调用）
#       check_line_endings.sh FILE...    # 只检查给定文件（自校准用）
set -u

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$ROOT" || exit 1

files=("$@")
if [ "${#files[@]}" -eq 0 ]; then
    # 只看 git 跟踪的文本文件；二进制（.png/.deb/.qcow2…）由 .gitattributes 标为 binary，
    # `--eol` 对它们报 `w/-text`，下面按 crlf 过滤即天然排除。
    while IFS= read -r f; do files+=("$f"); done < <(git ls-files)
fi

bad=0
for f in "${files[@]}"; do
    [ -f "$f" ] || continue
    eol=$(git ls-files --eol -- "$f" 2>/dev/null | head -1)
    # 未跟踪文件（git ls-files --eol 无输出）退化为直接查 CR
    if [ -z "$eol" ]; then
        if LC_ALL=C grep -qU $'\r' "$f" 2>/dev/null; then
            printf '✗ %s：含 CRLF/CR（未跟踪文件）\n' "$f"
            bad=$((bad + 1))
        fi
        continue
    fi
    case "$eol" in
        *"w/crlf"*)
            printf '✗ %s：工作区为 CRLF（仓库要求 LF，见 .gitattributes）\n' "$f"
            bad=$((bad + 1))
            ;;
    esac
done

if [ "$bad" -gt 0 ]; then
    echo "—— 共 $bad 个文件含 CRLF。修复：把行尾转回 LF（如 python: data.replace(b'\\r\\n', b'\\n')）"
    echo "   注意：git diff 看不出来（.gitattributes 的 eol=lf 会在提交时归一化），只有本守护抓得到。"
    exit 1
fi
echo "✓ 行尾一致（无 CRLF 污染）"

# ---- 自校准：已知 LF → 通过；已知 CRLF → 报违规（--selftest 时执行）----
if [ "${1:-}" = "--selftest" ]; then
    tmp=$(mktemp -d) || exit 1
    trap 'rm -rf "$tmp"' EXIT
    printf 'abc\ndef\n' > "$tmp/ok.txt"
    printf 'abc\r\ndef\r\n' > "$tmp/bad.txt"

    if bash "$0" "$tmp/ok.txt" >/dev/null 2>&1; then
        echo "  ✓ 自校准：已知 LF 文件判通过"
    else
        echo "  ✗ 自校准失败：已知 LF 文件被判违规（判据过严）"
        exit 1
    fi
    if bash "$0" "$tmp/bad.txt" >/dev/null 2>&1; then
        echo "  ✗ 自校准失败：已知 CRLF 文件被判通过（判据形同空转）"
        exit 1
    else
        echo "  ✓ 自校准：已知 CRLF 文件判违规"
    fi
    exit 0
fi
