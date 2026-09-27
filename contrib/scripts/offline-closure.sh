#!/usr/bin/env bash
# NFViS 离线安装包的**依赖闭包**工具（构建端使用，不随产品交付）。
#
# 两个模式：
#   check DIR
#       离线校验：DIR 里的 deb 集合必须**自洽**——每个 deb 的 Depends/Pre-Depends 都能被
#       集合内的包名或 Provides 满足（离线装机只能靠集合自己）。退出码 0=自洽、1=有缺口，
#       缺口逐条打印（谁需要、缺谁）。构建端不需要 apt、不需要联网。
#   fetch --out DIR --seed-deb FILE... [--pkg NAME...]
#       把种子 deb（产品包、VPP 等）连同依赖闭包**下载齐**：从种子出发递归解析
#       Depends/Pre-Depends，缺什么下什么，直到不再新增；最后自动跑一遍 check。
#
# 为什么要有 check：离线装机漏一个包，轻则装出"缺腿"的系统（如 VM 起不来、容器编排不可用），
#   重则整套失败；而"漏没漏"在构建端就能静态判定——不必等到气隙机器上试。
#
# 口径说明（刻意如此）：
#   · 只跟 Depends/Pre-Depends，**不跟 Recommends/Suggests**（可选项，缺了只降级功能）；
#   · `a | b` 多个可选分支只取第一个能解析的（先试 a，装不了再试 b 由 fetch 的下载失败反馈）；
#   · 版本约束不比较版本（集合里的包就是同一来源的同批候选），只要求"名字/Provides 能对上"；
#   · Essential/required 这些基础包**也一并下**——让集合自洽，不依赖"目标机应该有什么"的假设。
set -u

die() { printf '错误：%s\n' "$*" >&2; exit 2; }
have() { command -v "$1" >/dev/null 2>&1; }

deb_field() { dpkg-deb -f "$1" "$2" 2>/dev/null; }

# 依赖字段 → 需要的"供应者名"列表（逗号分段；每段取第一个可选分支；去版本约束与架构限定）
dep_names() {
    [ -n "${1:-}" ] || return 0
    printf '%s\n' "$1" | awk -F',' '{
        for (i = 1; i <= NF; i++) {
            s = $i
            sub(/\|.*/, "", s)
            sub(/[ \t]*\(.*/, "", s)
            sub(/:.*/, "", s)
            gsub(/^[ \t]+|[ \t]+$/, "", s)
            if (s != "") print s
        }
    }'
}

# 一个 deb 提供的名字（自己的包名 + Provides）
deb_provides() {
    deb_field "$1" Package
    dep_names "$(deb_field "$1" Provides)"
}

# ---------- check ----------
cmd_check() {
    local dir=${1:-}
    [ -n "$dir" ] || die "check 需要目录参数"
    [ -d "$dir" ] || die "目录不存在：$dir"
    have dpkg-deb || die "check 需要 dpkg-deb（任何 Ubuntu 都有）"

    local -a debs=()
    while IFS= read -r f; do debs+=("$f"); done < <(find "$dir" -maxdepth 1 -name '*.deb' -print | sort)
    [ "${#debs[@]}" -gt 0 ] || die "目录里没有 deb：$dir"

    declare -A HAVE=() COUNT=()
    local d n
    for d in "${debs[@]}"; do
        while IFS= read -r n; do
            [ -n "$n" ] || continue
            HAVE[$n]=1
            COUNT[$n]=$(( ${COUNT[$n]:-0} + 1 ))
        done < <(deb_provides "$d")
    done

    local missing=0 pairs=0 by
    local -a miss_lines=()
    for d in "${debs[@]}"; do
        for f in Depends Pre-Depends; do
            while IFS= read -r n; do
                [ -n "$n" ] || continue
                pairs=$((pairs + 1))
                if [ -z "${HAVE[$n]:-}" ]; then
                    by=$(deb_field "$d" Package)
                    miss_lines+=("$(printf '  · %s 需要 %s（集合内没有包名或 Provides 提供它）' "$by" "$n")")
                    missing=$((missing + 1))
                fi
            done < <(dep_names "$(deb_field "$d" "$f")")
        done
    done

    local size
    size=$(du -sh "$dir" 2>/dev/null | cut -f1)
    printf '闭包校验：%s\n' "$dir"
    printf '  包数 %d、依赖声明 %d 条、提供名 %d 个、合计 %s\n' "${#debs[@]}" "$pairs" "${#HAVE[@]}" "$size"
    if [ "$missing" -eq 0 ]; then
        printf '  [自洽] 全部依赖都能由集合自身满足\n'
        return 0
    fi
    printf '  [不自洽] %d 条依赖在集合内找不到提供者：\n' "$missing"
    printf '%s\n' "${miss_lines[@]}"
    return 1
}

# ---------- fetch ----------
apt_provider() { # 虚拟包 → 一个能装的实体包名（取第一个反向提供者）
    apt-cache showpkg "$1" 2>/dev/null | awk '/^Reverse Provides:/ { f = 1; next } f && NF >= 2 { print $1; exit }'
}

fetch_one() { # $1=名字 $2=输出目录；成功 0
    local n=$1 out=$2 prov
    if (cd "$out" && apt-get download "$n" >/dev/null 2>&1); then return 0; fi
    prov=$(apt_provider "$n")
    if [ -n "$prov" ] && (cd "$out" && apt-get download "$prov" >/dev/null 2>&1); then
        printf '  · %s 是虚拟包，改用提供者 %s\n' "$n" "$prov"
        return 0
    fi
    return 1
}

cmd_fetch() {
    local out=""
    local -a seeds=() pkgs=()
    while [ $# -gt 0 ]; do
        case "$1" in
            --out) shift; out=${1:-}; shift ;;
            --seed-deb) shift; [ -n "${1:-}" ] && seeds+=("$1"); shift ;;
            --pkg) # 后面可以跟多个包名（直到下一个 -- 选项）
                shift
                while [ $# -gt 0 ] && [ "${1#--}" = "$1" ]; do
                    pkgs+=("$1")
                    shift
                done
                ;;
            *) die "未知参数：$1" ;;
        esac
    done
    [ -n "$out" ] || die "fetch 需要 --out DIR"
    have dpkg-deb || die "fetch 需要 dpkg-deb"
    have apt-get || die "fetch 需要 apt-get（在联网的 Ubuntu 构建机上执行）"
    mkdir -p "$out"
    rm -f "$out/FETCH-FAILED.txt" # 重跑时重新记录（本工具可续跑：已在 out 里的 deb 会被视作已有）

    local s
    for s in "${seeds[@]}"; do
        [ -f "$s" ] || die "种子 deb 不存在：$s"
        cp -n "$s" "$out/" 2>/dev/null || true
    done

    {
        printf '来源（apt 源快照）\n'
        grep -rhoE 'https?://[^ ]+' /etc/apt/sources.list /etc/apt/sources.list.d/*.list \
            /etc/apt/sources.list.d/*.sources 2>/dev/null | sort -u | sed 's/^/  /'
    } > "$out/FETCH-SUMMARY.txt"

    declare -A SEEN=() HAVE=() TRIED=()
    local NEEDQ
    NEEDQ=$(mktemp)
    : > "$NEEDQ"

    scan() { # 登记未处理 deb 提供的名字，并收集其依赖需求
        local d n f
        for d in "$out"/*.deb; do
            [ -e "$d" ] || continue
            [ -n "${SEEN[$d]:-}" ] && continue
            SEEN[$d]=1
            while IFS= read -r n; do [ -n "$n" ] && HAVE[$n]=1; done < <(deb_provides "$d")
            for f in Depends Pre-Depends; do
                while IFS= read -r n; do
                    [ -n "$n" ] && printf '%s\t%s\n' "$n" "$(basename "$d")" >> "$NEEDQ"
                done < <(dep_names "$(deb_field "$d" "$f")")
            done
        done
    }

    local p
    for p in "${pkgs[@]}"; do printf '%s\t%s\n' "$p" "（命令行指定）" >> "$NEEDQ"; done

    local progress=1
    while [ "$progress" = 1 ]; do
        progress=0
        scan
        local n by
        while IFS=$'\t' read -r n by; do
            [ -n "$n" ] || continue
            [ -n "${HAVE[$n]:-}" ] && continue
            [ -n "${TRIED[$n]:-}" ] && continue
            TRIED[$n]=1
            if fetch_one "$n" "$out"; then
                progress=1
            else
                printf '%s\t需要者 %s\n' "$n" "$by" >> "$out/FETCH-FAILED.txt"
            fi
        done < "$NEEDQ"
    done

    rm -f "$NEEDQ"
    local got
    got=$(find "$out" -maxdepth 1 -name '*.deb' | wc -l)
    printf '闭包抓取：%s 个 deb\n' "$got"
    if [ -f "$out/FETCH-FAILED.txt" ]; then
        printf '抓不到的名字（%s 条，见 FETCH-FAILED.txt）：\n' "$(grep -c . "$out/FETCH-FAILED.txt")"
        sed 's/^/  · /' "$out/FETCH-FAILED.txt" | head -20
    fi
    cmd_check "$out"
}

case "${1:-}" in
    check) shift; cmd_check "$@" ;;
    fetch) shift; cmd_fetch "$@" ;;
    *) die "用法：offline-closure.sh check DIR | fetch --out DIR --seed-deb FILE... [--pkg NAME...]" ;;
esac
