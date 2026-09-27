#!/usr/bin/env bash
# 守护：维护脚本与安装器**不得在顶层 exit 之后还有可执行语句**。
#
# 由来（决策 #181）：deploy/debian/postinst 里 libvirt AppArmor 放行块曾被追加在 `exit 0` **之后**，
#   于是 `dpkg -i` 全程"成功"、一行报错都没有，放行却从未执行——干净快照首装的 VNF 因此起不来。
#   这类"写了但永远不执行"的缺陷在安装/卸载路径上最贵：只在真机首装暴露，而安装器还在报成功。
#   静态判据很便宜（下面 5 行 awk），没有理由不守。
#
# 判据：文件里第一个**顶格**（无缩进）的 exit 之后的非空、非注释行 = 违规。
#   只看顶格 exit：函数体与分支里的 exit 都有缩进，不具备"文件到此结束"的语义。
#
# 用法：check_maint_scripts.sh [FILE...]
#   缺省检查 deploy 下的维护脚本、安装期脚本与离线安装器。
# 自校准见 contrib/scripts/offline-installer-selftest.sh（已知正确→通过、已知错误→报违规）。
set -u

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
FILES=("$@")
if [ "${#FILES[@]}" -eq 0 ]; then
    FILES=(
        "$ROOT/deploy/debian/postinst" "$ROOT/deploy/debian/prerm" "$ROOT/deploy/debian/postrm"
        "$ROOT/deploy/offline/install.sh"
    )
    for f in "$ROOT"/deploy/installer/*.sh; do
        [ -e "$f" ] && FILES+=("$f")
    done
fi

bad=0
for f in "${FILES[@]}"; do
    [ -f "$f" ] || { printf '错误：找不到 %s\n' "$f" >&2; exit 2; }
    out=$(awk '
        term == 0 && $0 ~ /^exit([ \t]+[0-9]+)?[ \t]*$/ { term = 1; next }
        term == 1 && $0 !~ /^[ \t]*(#.*)?$/ { printf "%s:%d: %s\n", FILENAME, FNR, $0 }
    ' "$f")
    if [ -n "$out" ]; then
        printf '违规：%s 在顶层 exit 之后还有可执行语句（永远不会执行）：\n' "$f"
        printf '%s\n' "$out" | sed 's/^/  /'
        bad=1
    fi
done

if [ "$bad" = 0 ]; then
    printf '维护脚本检查：通过（%d 个文件，顶层 exit 之后无可执行语句）\n' "${#FILES[@]}"
fi
exit "$bad"
