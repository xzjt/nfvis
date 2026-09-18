#!/bin/sh
# NFViS 安装期 SSH 强化（FR-SEC-006）：**禁用 root 口令登录**（root 仍可用密钥登录）。
#
# 设计要点——每一条都是为了「别把操作者锁在门外」：
#   1) 只写 drop-in（sshd_config.d/99-nfvis.conf），**不改主 sshd_config**；drop-in 在主配置
#      的 PermitRootLogin 之前被 Include，OpenSSH「首个设定生效」故本文件优先；
#   2) 写之前先判「有没有非口令登录路径」（root 或任一普通用户存在**非空** authorized_keys）——
#      没有就**拒绝加固**并说明原因：宁可这次不做，也不能让下次 sshd 重启后再也进不来；
#   3) 写完跑 `sshd -t` 校验；不通过立即删除并报非零——不留下会导致 sshd 起不来的配置；
#   4) **不重启、不 reload sshd**：下一次 sshd 启动才生效，故不影响当前会话（安装期尤其重要）；
#   5) 幂等：内容未变不重写；
#   6) `--revert` 一键撤销。
#
# 用法：
#   nfvis-ssh-harden.sh --check     # 只报告：是否有非口令登录路径、是否已加固
#   nfvis-ssh-harden.sh --apply     # 落地（postinst 调用；失败不阻断安装）
#   nfvis-ssh-harden.sh --revert    # 撤销加固
set -eu

CONF="${NFVIS_SSH_CONF:-/etc/ssh/sshd_config.d/99-nfvis.conf}"
# 被搜索的「非口令登录路径」。可用 NFVIS_SSH_KEYS 覆盖——**仅供测试/影子部署**：
# 覆盖只会改变判定输入（脚本会把实际搜索的路径打印出来，便于发现被收窄）。
KEYS="${NFVIS_SSH_KEYS:-/root/.ssh/authorized_keys /home/*/.ssh/authorized_keys}"

log() { echo "nfvis-ssh-harden: $*"; }
die() { echo "nfvis-ssh-harden: $*" >&2; exit 1; }

MODE=""
while [ $# -gt 0 ]; do
    case "$1" in
        --check|--apply|--revert) MODE="$1" ;;
        *) die "未知参数: $1" ;;
    esac
    shift
done
[ -n "$MODE" ] || die "需指定 --check | --apply | --revert"

# has_key_login：是否存在非口令（密钥）登录路径。**这是唯一的防锁死闸门。**
has_key_login() {
    for f in $KEYS; do
        [ -s "$f" ] && return 0
    done
    return 1
}

content() {
    cat <<'EOF'
# NFViS 安装期 SSH 强化：禁用 root **口令**登录（root 仍可用密钥登录）
# 由 nfvis 包写入；撤销：删除本文件后重启 sshd，或执行
#   /usr/share/nfvis/installer/nfvis-ssh-harden.sh --revert
# 生效时机：**下一次 sshd 启动**（安装期不重启 sshd，避免切断当前会话）
PermitRootLogin prohibit-password
EOF
}

check() {
    log "搜索非口令登录路径：$KEYS"
    if has_key_login; then
        log "  找到密钥登录路径 → 可以安全加固"
    else
        log "  未找到（只有口令可登录）→ 加固会被拒绝"
    fi
    if [ -f "$CONF" ]; then
        log "已加固：$CONF"
        log "  当前生效值：$(sshd -T 2>/dev/null | grep -i '^permitrootlogin' || echo 取不到)"
    else
        log "未加固（$CONF 不存在）"
    fi
}

apply() {
    command -v sshd >/dev/null 2>&1 || { log "未安装 openssh-server（无 sshd），跳过"; return 0; }
    if [ -f "$CONF" ] && [ "$(cat "$CONF")" = "$(content)" ]; then
        log "已是最新，跳过"
        return 0
    fi
    if ! has_key_login; then
        log "跳过加固：未找到非口令登录路径（搜索范围：$KEYS）"
        log "  理由：加固后一旦 sshd 重启，仅凭口令将无法登录。"
        log "  处理：先给 root（或任一普通用户）配置 SSH 公钥，再执行 --apply。"
        return 0
    fi
    install -d -m 0755 "$(dirname "$CONF")"
    content > "$CONF"
    if ! sshd -t 2>/dev/null; then
        rm -f "$CONF"
        log "校验未通过（sshd -t 失败），已删除 $CONF，系统未改变"
        return 1
    fi
    log "已写入 $CONF：root 口令登录已禁用，root 密钥登录保留"
    log "  下一次 sshd 启动生效（安装期不重启 sshd）；撤销：bash $0 --revert"
}

revert() {
    if [ -f "$CONF" ]; then
        rm -f "$CONF"
        log "已撤销：删除 $CONF（重启 sshd 后恢复原状）"
    else
        log "无需撤销：$CONF 不存在"
    fi
}

case "$MODE" in
    --check) check ;;
    --apply) apply ;;
    --revert) revert ;;
esac
