#!/usr/bin/env bash
# NFViS 离线一键安装器（自解压包 .run 的载荷；也可单独执行：bash install.sh --payload <目录>）。
#
# 目标：在**无外网**的干净 Ubuntu 26.04 上，用包内自带的 deb 一步装齐 nfvis 与全部依赖，
#       把服务起起来，并做一轮带**独立事实源**的自检——自检不过就以非零退出码收场。
#
# 与产品边界的分工（刻意**不做**的事）：
#   · 不写 /etc/vpp/startup.conf —— 那是 nfvisd 的职责（`request vpp restart` 时按 committed 配置生成）。
#     本脚本只把发行版默认配置下的 VPP 起起来，让 nfvisd 能连上；业务口的 DPDK 绑定留给产品路径。
#   · 不改系统的 apt 源（本地源只用 `-o Dir::Etc::sourcelist=` 临时指定），不装任何包内没有的东西。
#   · 不碰业务网卡（不做 bind-dpdk、不改 IP、不动路由）。
#   · 内核启动基线由随包安装脚本按机器规格预置（1G 大页等，**需重启生效**），本脚本只核对与提示。
#
# 用法：nfvis-v<版本>.run [选项]
#   -y, --yes               跳过确认（自动化；非交互场景必给）
#       --admin-password P  预置 admin 口令（至少 8 个字符）。不给则由 nfvisd 生成随机一次性口令，
#                           首次启动后由本脚本从日志里取出来打印。
#       --no-start          只安装，不起服务（服务相关自检转为不可判定）
#       --verify            只体检（假定已安装；不安装）
#       --keep              保留解包目录（排查用；缺省成功即清理）
#       --payload DIR       载荷目录（.run 头部自动传；手工执行时指明）
#   -h, --help              显示帮助
#       --version           显示版本
#
# 退出码：0 全部通过（含不可判定项）；1 有失败项；2 用法或前置条件不满足。
set -u

EXPECT_VPP=${NFVIS_EXPECT_VPP:-26.06}

PAYLOAD=${NFVIS_PAYLOAD:-$(cd "$(dirname "$0")" && pwd)}
ASSUME_YES=0; DO_START=1; DO_INSTALL=1; KEEP=0; ADMIN_PW=""
CLI_PW=""; OTP=""; PW_SRC=""; PW_CONFIRMED=0
NFVIS_DB=${NFVIS_DB:-/var/lib/nfvis/nfvis.db}
FRESH_DB=1 # 配置库尚不存在 = 首次引导（--admin-password 与一次性口令都只在这种情况下出现）

PASS=0; FAILED=0; SKIPPED=0
FAILED_ITEMS=""

# ---- 输出 ----
hdr()  { printf '\n===== %s =====\n' "$1"; }
info() { printf '  · %s\n' "$1"; }
ok()   { printf '  [通过] %s\n' "$1"; PASS=$((PASS + 1)); }
bad()  { printf '  [失败] %s\n' "$1"; FAILED=$((FAILED + 1)); FAILED_ITEMS="$FAILED_ITEMS
  - $1"; }
skip() { printf '  [不可判定] %s\n' "$1"; SKIPPED=$((SKIPPED + 1)); }
log()  { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
die()  { printf '错误：%s\n' "$*" >&2; exit 2; }
have() { command -v "$1" >/dev/null 2>&1; }

# 随包内置的安装清单：与规格书/用户手册的底座清单一致，缺一个就装不出可用的一体机。
PKGS_INSTALL="nfvis vpp vpp-plugin-core vpp-plugin-dpdk vpp-drivers
libvirt-daemon-system libvirt-clients qemu-system-x86 qemu-utils cloud-image-utils
genisoimage docker.io chrony tcpdump curl ca-certificates"

usage() {
    cat <<'EOF'
用法：nfvis-v<版本>.run [选项]
  -y, --yes               跳过确认（自动化；非交互场景必给）
      --admin-password P  预置 admin 口令（至少 8 个字符，仅限字母数字与 @._+=-）
      --no-start          只安装，不起服务（服务相关自检转为不可判定）
      --verify            只体检（假定已安装；不安装）
      --keep              保留解包目录（排查用；缺省成功即清理）
      --payload DIR       载荷目录（.run 头部自动传；手工执行时指明）
  -h, --help              显示本帮助
      --version           显示版本
EOF
}

# ---------- 前置检查 ----------
preflight() {
    hdr "前置检查"
    [ "$(id -u)" = 0 ] || die "需以 root 运行（sudo ./nfvis-*.run）"
    [ -r /etc/os-release ] || die "读不到 /etc/os-release，无法判定发行版"
    # shellcheck disable=SC1091
    . /etc/os-release
    if [ "${ID:-}" != "ubuntu" ]; then
        die "本包只面向 Ubuntu（当前 ID=${ID:-未知}）；其它发行版请从源码构建"
    fi
    ARCH=$(dpkg --print-architecture 2>/dev/null || echo 未知)
    [ "$ARCH" = "amd64" ] || die "本包只面向 amd64（当前 ${ARCH}）"
    if [ "${VERSION_ID:-}" != "26.04" ]; then
        info "注意：目标平台是 Ubuntu 26.04，当前为 ${VERSION_ID:-未知}——继续，但底座差异自担"
    fi
    for t in dpkg apt-get sha256sum systemctl tar gzip sha256sum; do
        have "$t" || die "缺少必需命令：$t"
    done
    info "系统：$( . /etc/os-release; printf '%s' "${PRETTY_NAME:-unknown}") / $ARCH"
    if [ -s "$NFVIS_DB" ]; then FRESH_DB=0; fi
    if [ "$FRESH_DB" = 1 ]; then
        info "配置库：尚未建立（本次为首次引导）"
    else
        info "配置库：$NFVIS_DB 已存在（本次为升级/重装；既有账号与配置不受影响）"
    fi

    # 有别的包管理进程在跑时抢锁，会留下"装了一半"的状态——先挡掉
    for p in apt-get apt dpkg unattended-upgrade; do
        if pgrep -x "$p" >/dev/null 2>&1; then
            die "检测到 $p 正在运行（包管理锁被占用），请等它结束或先停掉再执行"
        fi
    done

    if [ "$DO_INSTALL" = 1 ]; then
        [ -d "$PAYLOAD" ] || die "载荷目录不存在：$PAYLOAD"
        [ -d "$PAYLOAD/debs" ] || die "载荷缺少 debs/ 目录：$PAYLOAD"
        [ -f "$PAYLOAD/SHA256SUMS" ] || die "载荷缺少 SHA256SUMS（完整性无法校验）"
        NDEB=$(find "$PAYLOAD/debs" -maxdepth 1 -name '*.deb' | wc -l)
        [ "$NDEB" -gt 0 ] || die "载荷里没有 deb：$PAYLOAD/debs"
        info "载荷：$NDEB 个 deb（$(du -sh "$PAYLOAD/debs" | cut -f1)）"
        log "校验载荷完整性（sha256）..."
        if (cd "$PAYLOAD" && sha256sum -c --quiet SHA256SUMS); then
            ok "载荷完整性：SHA256SUMS 逐文件一致"
        else
            die "载荷完整性校验失败（文件损坏或缺失）——请重新获取安装包"
        fi
        # 安装需要：deb 副本(1x) + 解包(约 2x) + apt 缓存；留足余量
        NEED_KB=$(( $(du -sk "$PAYLOAD/debs" | cut -f1) * 4 + 524288 ))
        for mnt in / /var; do
            FREE_KB=$(df -Pk "$mnt" | awk 'NR==2 {print $4}')
            if [ "$FREE_KB" -lt "$NEED_KB" ]; then
                die "$mnt 可用空间不足（需约 $((NEED_KB / 1024)) MB，当前 $((FREE_KB / 1024)) MB）"
            fi
        done
        info "空间：/ 与 /var 均满足（需约 $((NEED_KB / 1024)) MB）"
    fi
}

confirm() {
    [ "$ASSUME_YES" = 1 ] && return 0
    [ -t 0 ] || die "非交互场景请加 --yes"
    printf '\n即将安装 nfvis 及其依赖（本机 apt 源不会被修改，安装期间不访问网络）。继续？[y/N] '
    read -r ans
    case "$ans" in y | Y | yes | YES) return 0 ;; *) die "已取消" ;; esac
}

# ---------- 安装 ----------
WORK=""
prepare_work() {
    WORK=$(mktemp -d /var/tmp/nfvis-offline.XXXXXX) || die "无法创建临时目录"
    mkdir -p "$WORK/lists/partial" "$WORK/archives/partial"
    printf 'deb [trusted=yes] file:%s/debs ./\n' "$PAYLOAD" > "$WORK/sources.list"
}

apt_local() {
    apt-get \
        -o Dir::Etc::sourcelist="$WORK/sources.list" \
        -o Dir::Etc::sourceparts="-" \
        -o Dir::State::Lists="$WORK/lists" \
        -o Dir::Cache::archives="$WORK/archives" \
        -o APT::Get::List-Cleanup=0 \
        -o APT::Install-Recommends=0 \
        -o APT::Get::Assume-Yes=true \
        -o Acquire::Retries=0 \
        -o Dpkg::Options::=--force-confold \
        -o Dpkg::Options::=--force-confdef \
        -o Dpkg::Use-Pty=0 \
        "$@"
}

do_install() {
    hdr "安装依赖与产品包（本地源，离线）"
    DEBIAN_FRONTEND=noninteractive apt_local update > "$WORK/apt-update.log" 2>&1 ||
        { tail -20 "$WORK/apt-update.log"; die "本地源索引建立失败（见上）"; }
    ok "本地源索引：$(grep -c '^Package: ' "$WORK/lists"/*Packages 2>/dev/null | head -1) 个候选包"

    # shellcheck disable=SC2086
    PLAN=$(DEBIAN_FRONTEND=noninteractive apt_local -s install $PKGS_INSTALL 2>&1 | grep -E '^[0-9]+ upgraded|^E:' | tail -3)
    [ -n "$PLAN" ] && info "计划：$(printf '%s' "$PLAN" | tr '\n' ' ')"

    # shellcheck disable=SC2086
    if DEBIAN_FRONTEND=noninteractive apt_local install $PKGS_INSTALL > "$WORK/apt-install.log" 2>&1; then
        ok "apt 安装完成（$(grep -cE '^Setting up ' "$WORK/apt-install.log") 个包完成配置）"
    else
        printf '\n---- 安装日志尾部 ----\n' >&2
        tail -30 "$WORK/apt-install.log" >&2
        bad "apt 安装失败（完整日志：$WORK/apt-install.log）"
        return 1
    fi
    if [ -n "$(dpkg -C 2>&1)" ]; then
        dpkg -C
        bad "dpkg 审计有未收敛项（见上）"
    else
        ok "dpkg 审计：无半装/未配置项"
    fi
    return 0
}

# ---------- 起服务 ----------
wait_for() { # wait_for <秒> <命令...>
    local t=$1; shift
    local i=0
    while [ "$i" -lt "$t" ]; do
        if "$@" >/dev/null 2>&1; then return 0; fi
        sleep 1; i=$((i + 1))
    done
    return 1
}

http_code() { curl -sS -o /dev/null -m 5 -w '%{http_code}' "$@" 2>/dev/null || echo 000; }

ui_code() {
    if [ -f /var/lib/nfvis/tls/server.crt ]; then
        http_code --cacert /var/lib/nfvis/tls/server.crt https://127.0.0.1/api/v1/ui/
    else
        http_code -k https://127.0.0.1/api/v1/ui/
    fi
}

start_services() {
    hdr "启服务"
    # VPP：安装时保持"开机不自启"（发行版默认 startup.conf 只保证 VPP 起来，业务口的配置由
    # nfvisd 在 `request vpp restart` 时生成并接管）；这里手工起一次，让 nfvisd 立刻连上。
    if systemctl list-unit-files vpp.service >/dev/null 2>&1; then
        systemctl disable vpp.service >/dev/null 2>&1 && info "vpp.service 已设为开机不自启（由 nfvis 管理）"
        systemctl start vpp.service >/dev/null 2>&1 || info "VPP 启动失败（见自检项）"
    fi
    systemctl enable --now nfvis.service >/dev/null 2>&1 || info "nfvis.service 启动失败（见自检项）"
    log "等待 VPP 数据面套接字 ..."
    if wait_for 30 test -S /run/vpp/api.sock; then ok "VPP API 套接字就绪：/run/vpp/api.sock"; else info "VPP 套接字未就绪（见自检项）"; fi
    log "等待 nfvisd 控制面就绪 ..."
    if wait_for 60 test "$(ui_code)" = 200; then ok "控制面已监听且响应（HTTP 200）"; else info "控制面未就绪（见自检项）"; fi
}

grab_otp() { # 首次启动的一次性口令只在日志里出现一次
    local line
    line=$(journalctl -u nfvis.service --no-pager 2>/dev/null | grep '一次性口令' | tail -1) || return 1
    [ -n "$line" ] || return 1
    OTP=$(printf '%s' "$line" | sed -n 's/.*口令（仅显示一次，请立即修改）: *//p')
    [ -n "$OTP" ]
}

cli() { "$1" -server https://127.0.0.1 -u admin -p "$2" -source console -c "$3" 2>&1 | grep -v '^连接'; }
cli_is_err() { printf '%s\n' "$1" | grep -qE '^%%|^%[^%]|^%$|^校验失败'; }

# ---------- 自检（带独立事实源） ----------
run_checks() {
    hdr "自检"
    local ver want pkgver out

    pkgver=$(dpkg-query -W -f='${Status}|${Version}' nfvis 2>/dev/null || echo "|")
    want=""
    [ "$DO_INSTALL" = 1 ] && [ -f "$PAYLOAD/VERSION" ] && want=$(cat "$PAYLOAD/VERSION")
    if [ "${pkgver%%|*}" = "install ok installed" ]; then
        if [ -z "$want" ] || [ "${pkgver##*|}" = "$want" ]; then
            ok "nfvis 包：已安装，版本 ${pkgver##*|}"
        else
            bad "nfvis 包版本不符：期望 $want，实际 ${pkgver##*|}"
        fi
    else
        bad "nfvis 包未安装（dpkg 状态：${pkgver%%|*}）"
    fi

    if [ -z "$(dpkg -C 2>&1)" ]; then ok "dpkg 审计：无半装/未配置项"; else dpkg -C; bad "dpkg 审计有未收敛项"; fi

    # 下面这些项要有服务才有意义；--no-start 时如实记为不可判定
    if [ "$DO_START" != 1 ]; then
        skip "服务与运行态检查（--no-start：本次未起服务）"
        return 0
    fi

    if [ "$(systemctl is-active nfvis.service 2>/dev/null)" = "active" ] &&
        [ "$(systemctl is-enabled nfvis.service 2>/dev/null)" = "enabled" ]; then
        ok "nfvis.service：active 且开机自启"
    else
        bad "nfvis.service 状态异常（active=$(systemctl is-active nfvis.service 2>/dev/null)、enabled=$(systemctl is-enabled nfvis.service 2>/dev/null)）"
    fi

    if have vppctl && vppctl show version 2>/dev/null | grep -q "$EXPECT_VPP"; then
        ok "VPP 运行中（vppctl 独立事实源，版本含 $EXPECT_VPP）"
    else
        bad "VPP 未运行或版本不含 $EXPECT_VPP（vppctl show version 失败）"
    fi
    if [ -S /run/vpp/api.sock ]; then ok "VPP API 套接字：/run/vpp/api.sock"; else bad "VPP API 套接字缺失"; fi

    local code
    code=$(ui_code)
    if [ "$code" = 200 ]; then ok "控制台：GET /api/v1/ui/ 返回 200"; else bad "控制台无响应（HTTP $code）"; fi

    # 编排能力：libvirt 与 docker 就绪才有 VM/容器两条腿
    for svc in libvirtd docker; do
        if [ "$(systemctl is-active "$svc" 2>/dev/null)" = "active" ]; then
            ok "$svc.service：active"
        else
            bad "$svc.service 未运行（VM/容器编排能力不可用）"
        fi
    done

    # 回归守卫：libvirt 的 AppArmor 助手必须放行产品镜像/VM 路径，否则首装后 VNF 起不来
    local aa=/etc/apparmor.d/local/usr.lib.libvirt.virt-aa-helper
    if [ -f /etc/apparmor.d/usr.lib.libvirt.virt-aa-helper ]; then
        if grep -q '/var/lib/nfvis/images' "$aa" 2>/dev/null; then
            ok "AppArmor：已放行 /var/lib/nfvis 镜像与 VM 磁盘路径"
        else
            bad "AppArmor 未放行 /var/lib/nfvis（VNF 启动会被拒绝；见 $aa）"
        fi
    else
        skip "AppArmor 放行检查（本机没有 libvirt 的 AppArmor profile）"
    fi

    # 内核启动基线：随包安装脚本已按机器规格预置；本次不重启，故只核对写入与提示
    if [ -f /etc/default/grub.d/99-nfvis.cfg ]; then
        ok "内核启动基线片段：/etc/default/grub.d/99-nfvis.cfg"
        if grep -q 'hugepages' /proc/cmdline; then
            info "当前 cmdline 已含大页参数（本次启动即生效）"
        else
            info "大页参数需**重启**才对本次内核生效（当前 cmdline 未见 hugepages）"
        fi
    else
        bad "内核启动基线片段缺失（大页/隔离核无法生效）"
    fi
    for p in 1048576 2048; do
        local f=/sys/kernel/mm/hugepages/hugepages-${p}kB/nr_hugepages label
        case "$p" in
            1048576) label=1G ;;
            2048) label=2M ;;
            *) label="${p}K" ;;
        esac
        [ -r "$f" ] && info "$label 大页池：$(cat "$f") 页（free $(cat /sys/kernel/mm/hugepages/hugepages-${p}kB/free_hugepages 2>/dev/null)）"
    done

    # nfvis 自己的读物：需要口令，取不到就如实记不可判定（绝不把"没测到"算通过）
    # 口令来源与可靠性：
    #   · 首次引导（配置库尚不存在）：--admin-password 或本次生成的随机一次性口令 —— 可靠，
    #     此时登录失败是真失败；
    #   · 升级/重装：配置库里已有用户，那个口令不适用；只能用日志里的**历史**一次性口令试试
    #     （可能早被改过）—— 因此登录失败记不可判定，不拿它判"失败"（工具假红同样有害）。
    PW_RELIABLE=0
    if [ -n "$ADMIN_PW" ] && [ "$FRESH_DB" = 1 ]; then
        CLI_PW="$ADMIN_PW"; PW_SRC="本次预置的口令"; PW_RELIABLE=1
    elif grab_otp; then
        CLI_PW="$OTP"
        if [ "$FRESH_DB" = 1 ]; then PW_SRC="本次首次启动的一次性口令"; PW_RELIABLE=1
        else PW_SRC="日志里的历史一次性口令"; fi
    elif [ -n "$ADMIN_PW" ]; then
        PW_SRC=""
        info "配置库已存在（升级/重装）：本次未用 --admin-password 做自检，它只对首次引导生效"
    fi
    if [ -z "$CLI_PW" ]; then
        skip "CLI 经产品路径的读写检查（本机未提供口令且日志中无可用口令）"
        return 0
    fi
    out=$(cli nfvis-cli "$CLI_PW" "show version")
    if cli_is_err "$out"; then
        if [ "$PW_RELIABLE" = 1 ]; then
            bad "CLI show version 报错：$(printf '%s' "$out" | head -2 | tr '\n' ' ')"
        else
            skip "CLI 登录未成功（$PW_SRC 可能已过期）：$(printf '%s' "$out" | head -1)"
        fi
        return 0
    fi
    if [ -n "$want" ] && printf '%s' "$out" | grep -q "$want"; then
        ok "CLI show version：$want（经 nfvisd 的 API 通路，口令取自$PW_SRC）"
        PW_CONFIRMED=1
    else
        info "CLI show version 输出未含期望版本（$want），原文：$(printf '%s' "$out" | head -2 | tr '\n' ' ')"
        ok "CLI 可用（show version 有应答，口令取自$PW_SRC）"
        PW_CONFIRMED=1
    fi
    out=$(cli nfvis-cli "$CLI_PW" "show vpp")
    if cli_is_err "$out"; then
        bad "CLI show vpp 报错：$(printf '%s' "$out" | head -2 | tr '\n' ' ')"
    elif printf '%s' "$out" | grep -q "$EXPECT_VPP"; then
        ok "CLI show vpp：nfvisd 已连上 VPP（$EXPECT_VPP）"
    else
        bad "CLI show vpp 未反映 VPP 连接（原文：$(printf '%s' "$out" | head -3 | tr '\n' ' ')）"
    fi
    return 0
}

# ---------- 收尾 ----------
summary() {
    hdr "结果"
    local admin_note=""
    if [ "$DO_INSTALL" = 1 ]; then
        if [ -n "$ADMIN_PW" ] && [ "$FRESH_DB" = 1 ]; then
            admin_note="admin 口令：按 --admin-password 预置（请尽快改：nfvis-cli 登录后 request system password change）"
        elif [ -n "$OTP" ]; then
            if [ "$FRESH_DB" = 1 ]; then
                admin_note="admin 口令（首次启动的一次性口令，仅此一处）：$OTP"
            elif [ "$PW_CONFIRMED" = 1 ]; then
                admin_note="既有 admin 口令（取自日志，本次已用它登录 CLI 验证）：$OTP"
            else
                admin_note="日志里最近的一次性口令是 $OTP（本次未验证；若口令已改过，请以你手上的为准）"
            fi
        fi
    fi
    [ -n "$admin_note" ] && printf '%s\n' "$admin_note"
    printf '自检：通过 %d / 失败 %d / 不可判定 %d\n' "$PASS" "$FAILED" "$SKIPPED"
    if [ "$FAILED" -gt 0 ]; then
        printf '失败项：%s\n' "$FAILED_ITEMS"
        printf '\n未通过——请按上方失败项处理后重跑：./nfvis-*.run --verify\n'
        return 1
    fi
    cat <<'EOF'

下一步：
  1) 重启一次，让内核启动基线（大页/隔离核/IOMMU）生效：systemctl reboot
  2) 登录 CLI 规划资源与业务口（wizard 会问几问）：nfvis-cli
  3) 浏览器打开控制台：https://<本机管理地址>/api/v1/ui/

说明：业务网卡尚未交给数据面——按用户手册的数据面章节声明接口后再绑定（管理口有守卫，不会被误绑）。
EOF
    return 0
}

main() {
    while [ $# -gt 0 ]; do
        case "$1" in
            -y | --yes) ASSUME_YES=1 ;;
            --admin-password) shift; ADMIN_PW="${1:-}"; [ -n "$ADMIN_PW" ] || die "--admin-password 需要值" ;;
            --no-start) DO_START=0 ;;
            --verify) DO_INSTALL=0 ;;
            --keep) KEEP=1 ;;
            --payload) shift; PAYLOAD="${1:-}"; [ -n "$PAYLOAD" ] || die "--payload 需要目录" ;;
            -h | --help) usage; return 0 ;;
            --version) printf 'nfvis offline installer %s\n' "$(cat "$PAYLOAD/VERSION" 2>/dev/null || echo unknown)"; return 0 ;;
            *) die "未知选项：$1（-h 看用法）" ;;
        esac
        shift
    done
    if [ -n "$ADMIN_PW" ]; then
        [ "${#ADMIN_PW}" -ge 8 ] ||
            die "--admin-password 至少 8 个字符（短于 8 会被 nfvisd 的口令策略拒绝、守护进程起不来）"
        # 口令要穿过 systemd 单元文件（Environment=），引号/百分号/反斜杠/美元符的转义语义
        # 在 systemd 里与 shell 不同，宁可在入口挡住也不留"看着设了、其实被改写"的坑。
        case "$ADMIN_PW" in
            *[!A-Za-z0-9@._+=-]*)
                die "--admin-password 含受限字符（只接受字母、数字与 @ . _ + = -），请换一个；产品或自动生成的口令都满足该口径"
                ;;
        esac
    fi

    preflight
    if [ "$DO_INSTALL" = 1 ]; then
        confirm
        prepare_work
        do_install || { summary >/dev/null; printf '安装未完成——见上方日志。\n' >&2; return 1; }
        if [ -n "$ADMIN_PW" ] && [ "$FRESH_DB" = 1 ]; then
            install -d -m 0755 /etc/systemd/system/nfvis.service.d
            cat > /etc/systemd/system/nfvis.service.d/10-offline-init-password.conf <<EOF
[Service]
Environment="NFVIS_OFFLINE_INIT_PW=$ADMIN_PW"
ExecStart=
ExecStart=/usr/bin/nfvisd -db \${NFVIS_DB} -listen \${NFVIS_LISTEN} -vpp-sock \${NFVIS_VPP_SOCK} -init-admin-password \${NFVIS_OFFLINE_INIT_PW}
EOF
            systemctl daemon-reload
            info "已按 --admin-password 预置首次引导口令（该 drop-in 在用户建立后即删除）"
        elif [ -n "$ADMIN_PW" ]; then
            info "配置库已存在：--admin-password 只对首次引导生效，本次不写 drop-in（口令沿用既有库）"
        fi
        if [ "$DO_START" = 1 ]; then
            start_services
            if [ -n "$ADMIN_PW" ] && [ "$FRESH_DB" = 1 ]; then
                rm -f /etc/systemd/system/nfvis.service.d/10-offline-init-password.conf
                rmdir /etc/systemd/system/nfvis.service.d 2>/dev/null || true
                systemctl daemon-reload
            fi
        fi
    else
        info "只体检模式（不安装、不起服务）"
    fi
    run_checks
    summary || return 1
    if [ "$DO_INSTALL" = 1 ] && [ "$KEEP" != 1 ] && [ -n "$WORK" ]; then
        rm -rf "$WORK"
    elif [ -n "$WORK" ]; then
        info "解包目录保留：$WORK"
    fi
    return 0
}

main "$@"
exit $?
