#!/usr/bin/env bash
# NFViS 开发验证虚机初始化脚本（目标：纯净 Ubuntu 26.04 Server，root 运行）
#
# 用法（在虚机上以 root 执行）：
#   PROXY=http://192.168.155.1:2333 ./provision.sh            # 全量安装（走代理）
#   PROXY=socks5://192.168.155.1:2333 ./provision.sh          # socks5 代理
#   HUGEPAGES_1G=4 ./provision.sh                             # 自定义 1G 大页数量（默认 4，需 reboot 生效）
#   SKIP_VPP=1 SKIP_DOCKER=1 ./provision.sh                   # 跳过组件（幂等，可重复执行）
#
# 行为：幂等；每步失败仅记录并继续，结尾汇总 [OK]/[FAIL] 清单；日志同时在 /var/log/nfvis-provision.log
set -u
export DEBIAN_FRONTEND=noninteractive
LOG=/var/log/nfvis-provision.log
PROXY="${PROXY:-}"
HUGEPAGES_1G="${HUGEPAGES_1G:-4}"
GO_VERSION="${GO_VERSION:-1.26.3}"
RESULTS=()

log() { echo "[$(date +%T)] $*" | tee -a "$LOG"; }
step() { # step <名称> <命令...>：执行并记录结果
    local name="$1"; shift
    log ">>> ${name} 开始"
    if "$@" >>"$LOG" 2>&1; then
        RESULTS+=("[OK]   ${name}"); log "<<< ${name} 成功"
    else
        RESULTS+=("[FAIL] ${name} （详见 $LOG）"); log "<<< ${name} 失败"
    fi
}

[ "$(id -u)" = 0 ] || { echo "必须以 root 运行"; exit 1; }
touch "$LOG"

# ---- 代理（按需启用：apt / go / docker pull / curl 统一走 PROXY）----
if [ -n "$PROXY" ]; then
    log "启用代理: ${PROXY}"
    cat >/etc/apt/apt.conf.d/95nfvis-proxy <<EOF
Acquire::http::Proxy "${PROXY}";
Acquire::https::Proxy "${PROXY}";
EOF
    export http_proxy="$PROXY" https_proxy="$PROXY" HTTP_PROXY="$PROXY" HTTPS_PROXY="$PROXY"
    # 尊重 socks5:// 形式（curl/go 原生支持；apt 走 http 代理头亦可由代理服务器转换）
    export ALL_PROXY="$PROXY"
else
    log "未配置代理（PROXY 变量为空）"
fi

# ---- 1. 基础包 ----
step "apt update"        bash -c 'apt-get -y update'
step "基础工具包"        bash -c 'apt-get -y install git curl wget ca-certificates build-essential pkg-config unzip gnupg lsb-release jq socat'
# M4 依赖：qemu-utils（qemu-img：建盘/快照/格式转换）、cloud-image-utils（cloud-localds：NoCloud seed ISO）、
# genisoimage（seed ISO 备选路径）、tcpdump（vhost-user/memif 通流排障）
step "虚拟化辅助工具"    bash -c 'apt-get -y install qemu-utils cloud-image-utils genisoimage tcpdump'

# ---- 2. Go 工具链（固定版本 tarball，不依赖发行版包）----
step "安装 Go ${GO_VERSION}" bash -c '
    set -e
    if [ -x /usr/local/go/bin/go ] && /usr/local/go/bin/go version | grep -q "go'"${GO_VERSION}"'"; then
        echo "Go 已安装，跳过"; exit 0
    fi
    curl -fsSL -o /tmp/go.tgz "https://go.dev/dl/go'"${GO_VERSION}"'.linux-amd64.tar.gz"
    rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz
    ln -sf /usr/local/go/bin/go /usr/local/bin/go
    ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
    go version'

# ---- 3. VPP 26.06（fd.io release 仓库；发行版 codename 缺失时回退 noble）----
step "安装 VPP" bash -c '
    set -e
    if command -v vppctl >/dev/null 2>&1 && vppctl show version 2>/dev/null | grep -q 26.06; then
        echo "VPP 已安装，跳过"; exit 0
    fi
    CODENAME=$(grep VERSION_CODENAME /etc/os-release | cut -d= -f2)
    try_repo() {  # $1 = 使用的工作代号
        curl -fsSL "https://packagecloud.io/fdio/release/gpgkey" | gpg --dearmor >/etc/apt/keyrings/fdio.gpg 2>/dev/null
        echo "deb [signed-by=/etc/apt/keyrings/fdio.gpg] https://packagecloud.io/fdio/release/ubuntu ${1} main" >/etc/apt/sources.list.d/fdio.list
        apt-get -y update && apt-get -y install vpp vpp-plugin-core vpp-plugin-dpdk
    }
    mkdir -p /etc/apt/keyrings
    try_repo "$CODENAME" || { rm -f /etc/apt/sources.list.d/fdio.list; try_repo noble; }
    # 开发虚机：VPP 由 nfvisd/手工管理，systemd 不自启（避免自动抢占网卡）
    systemctl disable vpp 2>/dev/null || true'

# ---- 4. Libvirt + QEMU/KVM ----
step "安装 Libvirt/QEMU" bash -c '
    set -e
    apt-get -y install qemu-system-x86 libvirt-daemon-system libvirt-clients virtinst
    systemctl enable --now libvirtd
    # 开发虚机需嵌套虚拟化时，kvm 模块由内核提供；缺失仅提示，不视为失败
    ls -l /dev/kvm 2>/dev/null || echo "提示：无 /dev/kvm（嵌套虚拟化未开启），M4 需在宿主开启"'

# ---- 5. Docker ----
step "安装 Docker" bash -c '
    set -e
    if command -v docker >/dev/null 2>&1; then echo "Docker 已安装，跳过"; exit 0; fi
    apt-get -y install docker.io
    systemctl enable --now docker'

# ---- 6. 大页内存（1G 页，reboot 生效；同步 selinux=0 无关项不动）----
step "配置大页内存（复用产品安装器脚本，与 CLI 同一生成器）" bash -c '
    set -e
    SRC=/opt/nfvis/src/nfvis/deploy/installer/nfvis-baseline.sh
    NFVISD=/usr/local/go/bin/go   # 开发机上 nfvisd 由 go run 提供，改用源内生成器
    if [ -x "$SRC" ]; then
        # 开发虚机：直接用 go run 生成 + 落地（与安装器脚本同源逻辑）
        cd /opt/nfvis/src/nfvis
        OUT=$(go run ./cmd/nfvisd -print-kernel-baseline --hugepages-1g "$HUGEPAGES_1G")
        FRAG_NEW=$(printf "%s
" "$OUT" | sed "/^---FSTAB---$/,$d")
        FSTAB_NEW=$(printf "%s
" "$OUT" | sed -n "/^---FSTAB---$/,$p" | sed "1d")
        mkdir -p /etc/default/grub.d
        printf "%s
" "$FRAG_NEW" > /etc/default/grub.d/99-nfvis.cfg
        grep -v -e "# nfvis-hugepages" -e "/dev/hugepages" /etc/fstab > /etc/fstab.tmp || true
        printf "# nfvis-hugepages
%s
" "$FSTAB_NEW" >> /etc/fstab.tmp
        mv -f /etc/fstab.tmp /etc/fstab
        update-grub
    else
        echo "警告：未找到 $SRC，跳过内核基线"
    fi
    mkdir -p /dev/hugepages
    grep -h nfvis /etc/default/grub.d/99-nfvis.cfg 2>/dev/null || true' 
