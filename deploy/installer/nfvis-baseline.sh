#!/bin/sh
# NFViS 安装期内核启动基线（FR-SYS-014 / 决策 #66）。
#
# 设计要点：**与 CLI 同一生成器** —— GRUB 片段与 fstab 行由 `nfvisd -print-kernel-baseline`
# 生成（Go 侧 internal/system.GenerateBaseline），本脚本只负责落地，避免"安装器一套、
# CLI 另一套"的双源漂移（历史缺陷类型见 docs/reviews/2026-09-13.md）。
#
# 用法：
#   nfvis-baseline.sh --check                      # 只检查并报告（postinst 用），不写系统
#   nfvis-baseline.sh --apply --hugepages-1g 8 [--isolated-cores 4-15] [--thp never] \
#                     [--iommu pt] [--low-latency] [--tuned-profile nfvis-throughput] [--params "intel_iommu=on"]
#   nfvis-baseline.sh --defaults                   # 保守默认：1G 页 = min(RAM_GB/4, 8)，不设 isolcpus
#   nfvis-baseline.sh --rollback                   # 恢复上一次片段（无备份则删除片段）
#
# 生成器会按本机事实自动补全（补全值与显式参数同名冲突时以显式为准）：
#   - CPU 厂商参数（intel_iommu=on intel_pstate=disable / amd_iommu=on amd_pstate=disable + iommu=pt）；
#   - 设了隔离核时补 irqaffinity=<非隔离核>（中断亲和到非隔离核）；
#   - 内核无 nohz_full 支持（未编入 CONFIG_NO_HZ_FULL）时省略 nohz_full/rcu_nocbs。
# 护栏：隔离核必须落在本机在线核内，且非隔离核至少保留 2 个（内核/中断/管理面），
#       否则生成被拒绝——isolcpus 写错要重启才会暴露，且是进不了系统级别。
#
# 幂等：内容不变则不重写、不跑 update-grub；写入前备份到 /var/lib/nfvis/kernel-baseline.bak；
# update-grub 失败自动回退片段，不留下未生效的 GRUB 配置。变更需重启生效。
set -eu

NFVISD="${NFVISD:-/usr/bin/nfvisd}"
FRAG="/etc/default/grub.d/99-nfvis.cfg"
BAK="/var/lib/nfvis/kernel-baseline.bak"
FSTAB="/etc/fstab"
MARK="# nfvis-hugepages"
# 生成器 stderr 的暂存（apply 时把校验失败原因原样带给操作者，而不是一句笼统的"失败"）
ERRFILE=$(mktemp)
trap 'rm -f "$ERRFILE"' EXIT

log() { echo "nfvis-baseline: $*"; }
die() { echo "nfvis-baseline: $*" >&2; exit 1; }

MODE=""
HP1G=0; HP2M=0; ISO=""; THP=""; IOMMU=""; LOWLAT="no"; TUNED=""; PARAMS=""

while [ $# -gt 0 ]; do
    case "$1" in
        --check|--apply|--defaults|--rollback) MODE="$1" ;;
        --hugepages-1g) HP1G="$2"; shift ;;
        --hugepages-2m) HP2M="$2"; shift ;;
        --isolated-cores) ISO="$2"; shift ;;
        --thp) THP="$2"; shift ;;
        --iommu) IOMMU="$2"; shift ;;
        --low-latency) LOWLAT="yes" ;;
        --tuned-profile) TUNED="$2"; shift ;;
        --params) PARAMS="$2"; shift ;;
        *) die "未知参数: $1" ;;
    esac
    shift
done
[ -n "$MODE" ] || die "需指定 --check | --apply | --defaults | --rollback"

# 1) 报告当前一致性（读 /proc，不写系统）
check() {
    log "内核基线检查："
    log "  本机：CPU 厂商 $(awk -F': ' '/^vendor_id/{print $2; exit}' /proc/cpuinfo 2>/dev/null || echo 未知)、在线核 $(cat /sys/devices/system/cpu/online 2>/dev/null || echo 未知)、nohz_full 支持 $([ -e /sys/devices/system/cpu/nohz_full ] && echo 是 || echo 未见)"
    if [ -f "$FRAG" ]; then
        log "  片段存在：$FRAG"
        log "  期望参数：$(grep -o 'GRUB_CMDLINE_LINUX=.*' "$FRAG" | head -1)"
    else
        log "  片段缺失（未托管内核基线）"
    fi
    log "  运行 cmdline：$(tr ' ' '\n' < /proc/cmdline | grep -E 'hugepages|isolcpus|nmi_watchdog|transparent_hugepage' | tr '\n' ' ')"
    log "  大页实际：$(grep -i '^HugePages_Total' /proc/meminfo | tr -s ' ')  空闲 $(grep -i '^HugePages_Free' /proc/meminfo | awk '{print $2}')"
    log "  分尺寸：1G=$(cat /sys/kernel/mm/hugepages/hugepages-1048576kB/nr_hugepages 2>/dev/null || echo 未知)  2M=$(cat /sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages 2>/dev/null || echo 未知)"
    if grep -q "$MARK" "$FSTAB" 2>/dev/null; then
        log "  fstab 大页挂载：$(grep -A1 "$MARK" "$FSTAB" | tail -1)"
    else
        log "  fstab 大页挂载：缺失"
    fi
}

# 2) 保守默认：1G 页 = min(RAM_GB/4, 8)；不设 isolcpus（留待 CLI/resource-pools 按 VPP worker 规划）
defaults() {
    RAM_KB=$(awk '/^MemTotal/{print $2}' /proc/meminfo)
    RAM_GB=$((RAM_KB / 1024 / 1024))
    HP1G=$((RAM_GB / 4))
    [ "$HP1G" -gt 8 ] && HP1G=8
    [ "$HP1G" -lt 1 ] && HP1G=1
    log "按机器规格取默认：RAM ${RAM_GB}G → 1G 大页 ${HP1G} 页（不设 isolcpus）"
}

# 3) 落地（生成器 → 片段/fstab → update-grub，失败回退）
apply() {
    [ -x "$NFVISD" ] || die "找不到 $NFVISD（先安装 nfvis 包，或用 NFVISD=… 指定）"

    # 一次性迁移：若主 grub 文件里已有 nfvis 托管的参数（老装机方式留下的），
    # 先摘除，避免与片段重复注入同一参数（否则 cmdline 出现两次、以最后一次为准）。
    if [ -f /etc/default/grub ] && grep -qE 'hugepagesz=|isolcpus=' /etc/default/grub; then
        cp -f /etc/default/grub /etc/default/grub.nfvis-bak 2>/dev/null || true
        sed -i -E 's/ ?(default_hugepagesz|hugepagesz|hugepages|isolcpus|nohz_full|rcu_nocbs|nmi_watchdog|transparent_hugepage)=[^ "]*//g' /etc/default/grub
        log "已从 /etc/default/grub 摘除旧的 nfvis 内核参数（备份 /etc/default/grub.nfvis-bak），改由片段统一托管"
    fi

    GEN_ARGS="--print-kernel-baseline --hugepages-1g $HP1G --hugepages-2m $HP2M"
    [ -n "$ISO" ] && GEN_ARGS="$GEN_ARGS --isolated-cores $ISO"
    [ -n "$THP" ] && GEN_ARGS="$GEN_ARGS --thp $THP"
    [ -n "$IOMMU" ] && GEN_ARGS="$GEN_ARGS --iommu $IOMMU"
    [ "$LOWLAT" = "yes" ] && GEN_ARGS="$GEN_ARGS -low-latency"
    [ -n "$TUNED" ] && GEN_ARGS="$GEN_ARGS --tuned-profile $TUNED"
    [ -n "$PARAMS" ] && GEN_ARGS="$GEN_ARGS --kernel-params \"$PARAMS\""

    [ "$LOWLAT" = "yes" ] && log "低延迟参数组已启用：mitigations=off 等会降低安全缓解与可诊断性，idle=poll 使核常驻满载（虚拟机上自动省略 idle=poll/tsc=reliable）"
    OUT=$(eval "$NFVISD $GEN_ARGS" 2>"$ERRFILE") || die "$(cat "$ERRFILE")"
    [ -s "$ERRFILE" ] && log "生成器警告：$(cat "$ERRFILE")"
    rm -f "$ERRFILE"
    FRAG_NEW=$(printf '%s\n' "$OUT" | sed '/^---FSTAB---$/,$d')
    FSTAB_NEW=$(printf '%s\n' "$OUT" | sed -n '/^---FSTAB---$/,$p' | sed '1d')

    mkdir -p "$(dirname "$FRAG")" "$(dirname "$BAK")"
    if [ -f "$FRAG" ]; then
        if [ "$(cat "$FRAG")" = "$FRAG_NEW" ]; then
            log "片段内容未变，跳过写入与 update-grub"
        else
            cp -f "$FRAG" "$BAK"
            printf '%s\n' "$FRAG_NEW" > "$FRAG"
            log "片段已更新（上一版备份：$BAK）"
        fi
    else
        printf '%s\n' "$FRAG_NEW" > "$FRAG"
        log "片段已写入：$FRAG"
    fi

    # fstab 大页挂载行（幂等：按标记整行替换）
    if [ -f "$FSTAB" ]; then
        grep -v -e "$MARK" -e "/dev/hugepages" "$FSTAB" > "$FSTAB.nfvis-tmp" || true
        printf '%s\n%s\n' "$MARK" "$FSTAB_NEW" >> "$FSTAB.nfvis-tmp"
        mv -f "$FSTAB.nfvis-tmp" "$FSTAB"
        log "fstab 大页挂载已维护"
    else
        printf '%s\n%s\n' "$MARK" "$FSTAB_NEW" > "$FSTAB"
    fi

    if ! update-grub >/dev/null 2>&1; then
        if [ -f "$BAK" ]; then cp -f "$BAK" "$FRAG"; else rm -f "$FRAG"; fi
        die "update-grub 失败，已回退片段（系统未改变）"
    fi

    # R88-1/#201：大页池 sysctl 的唯一真源。VPP 包自带 /etc/sysctl.d/80-vpp.conf
    # （vm.nr_hugepages=1024，注释写明是给 2M 池的），而该 sysctl 只作用于**默认尺寸**池——
    # 本脚本设了 default_hugepagesz=1G 时它就落到 1G 池上，开机按可用内存尽量分配，
    # 1G 池会大于这里的声明值（真机 round88：cmdline hugepages=1、实际 nr=4，于是
    # show system kernel 长期显示「基线 1 / 实际 4」不一致）。安装期由 postinst 用
    # dpkg-divert 把 vpp 那个 conffile 挪开（决策 #201），本文件按声明值写
    # vm.nr_hugepages（并接管 vpp 原文件里的 vm.hugetlb_shm_group=0）。
    # 生成器与运行期（nfvisd 的内核基线 Apply / 启动补写）共用同一个
    # `nfvisd --print-hugepage-sysctl`，避免两处各写一份而漂移。
    SYSCTL="/etc/sysctl.d/90-nfvis-hugepages.conf"
    # 决策 #347：vm.nr_hugepages 作用于**当前内核默认尺寸池**（cmdline 的 default_hugepagesz）。
    # 取不到默认尺寸时保守不动（不写不删）——不猜，也不把某尺寸的声明值喂给未知的池。
    # 优先取**即将生效**的基线（刚写入的 GRUB 片段），回退当前运行 cmdline——
    # apply 后重启前运行 cmdline 仍是旧基线，按它取会把值钉给错的默认尺寸池（真机 round127）。
    DEFSZ=$(grep -oE 'default_hugepagesz=[0-9]+[MG]' "$FRAG" 2>/dev/null | head -1 | cut -d= -f2 || true)
    if [ -z "$DEFSZ" ]; then
        DEFSZ=$(grep -oE 'default_hugepagesz=[0-9]+[MG]' /proc/cmdline 2>/dev/null | head -1 | cut -d= -f2 || true)
    fi
    if [ -z "$DEFSZ" ]; then
        log "未取到内核默认大页尺寸（default_hugepagesz），保守不动大页池 sysctl 片段：$SYSCTL"
    else
        SYSCTL_NEW=$("$NFVISD" --print-hugepage-sysctl --hugepages-1g "$HP1G" --hugepages-2m "$HP2M" 2>/dev/null || true)
        if [ -n "$SYSCTL_NEW" ]; then
            mkdir -p "$(dirname "$SYSCTL")"
            if [ -f "$SYSCTL" ] && [ "$(cat "$SYSCTL")" = "$SYSCTL_NEW" ]; then
                log "大页池 sysctl 片段未变，跳过写入"
            else
                printf '%s\n' "$SYSCTL_NEW" > "$SYSCTL"
                log "大页池 sysctl 已写入：$SYSCTL（$(printf '%s\n' "$SYSCTL_NEW" | tail -1)）"
            fi
        else
            rm -f "$SYSCTL"
        fi
    fi

    log "update-grub 成功；**需重启生效**：重启后 show system kernel 应显示与配置一致"
}

rollback() {
    # 大页池 sysctl 片段同为本脚本产物：回退时一并撤除（否则它会继续把默认尺寸池钉在旧值上）
    rm -f /etc/sysctl.d/90-nfvis-hugepages.conf
    if [ -f "$BAK" ]; then
        cp -f "$BAK" "$FRAG"
        log "已恢复上一次片段（$BAK）"
    else
        rm -f "$FRAG"
        log "无备份，已删除片段（回到系统原始启动参数）"
    fi
    update-grub >/dev/null 2>&1 || die "update-grub 失败"
    log "update-grub 成功；需重启生效"
}

case "$MODE" in
    --check) check ;;
    --defaults) defaults; apply ;;
    --apply) apply ;;
    --rollback) rollback ;;
esac
