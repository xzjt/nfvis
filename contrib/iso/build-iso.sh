#!/usr/bin/env bash
# NFViS 安装 ISO 构建（决策 #110）：Ubuntu Server live ISO remaster + autoinstall + 离线底座闭包。
#
# 用法（Linux 构建机 / nfvis-vm）：
#   make iso VERSION=1.1.20                      # 先构建 nfvis deb 再 remaster
#   NFVIS_DEB=path/to/nfvis_x.y.z_amd64.deb bash contrib/iso/build-iso.sh  # 复用已有 deb
#
# 输入（环境变量，缺省值即验证机 nfvis-vm 的现场路径；构建不联网）：
#   ISO_SRC        官方 live-server ISO（须已对官方 SHA256SUMS 校验）
#   VPP_DEBS       VPP deb 目录（非 Ubuntu 源，必须随包）
#   CLOSURE_DEBS   离线闭包目录（docker/libvirt/qemu/cloud-image-utils 递归闭包，
#                  应含补齐脚本落下的 closure.done 标记）
#   NFVIS_DEB      nfvis deb（缺省 build/nfvis_VERSION_ARCH.deb）
#   NFVIS_PASSWORD 装机后 OS 用户 nfvis 的口令（构建时生成 crypt 哈希注入 seed）
#   ISO_OUT        输出路径（缺省 build/nfvis_VERSION_ARCH.iso）
#
# 产物结构：官方 ISO 原位复制 + /nfvis/{user-data,meta-data,debs/} +
# boot/grub/grub.cfg 追加 opt-in 菜单项「NFViS 自动安装（将清空所选磁盘）」，
# 缺省引导条目仍是原 Ubuntu（不选中即不清盘）。
set -euo pipefail

VERSION="${VERSION:?需设置 VERSION（make iso VERSION=…）}"
ARCH="${ARCH:-amd64}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

ISO_SRC="${ISO_SRC:-/root/isobuild/ubuntu-26.04.1-live-server-amd64.iso}"
VPP_DEBS="${VPP_DEBS:-/root/vpp-v26.06-deb}"
CLOSURE_DEBS="${CLOSURE_DEBS:-/root/isobuild/debs}"
NFVIS_DEB="${NFVIS_DEB:-build/nfvis_${VERSION}_${ARCH}.deb}"
NFVIS_PASSWORD="${NFVIS_PASSWORD:-Nfvis@Test2026}"
ISO_OUT="${ISO_OUT:-build/nfvis_${VERSION}_${ARCH}.iso}"
STAGING="build/iso-staging"
FRAGMENT=deploy/iso/grub-autoinstall-entry.cfg

log() { echo "build-iso: $*"; }
die() { echo "build-iso: ✗ $*" >&2; exit 1; }

command -v xorriso >/dev/null 2>&1 || die "需要 xorriso"
command -v openssl >/dev/null 2>&1 || die "需要 openssl（生成口令 crypt 哈希）"
command -v dpkg-deb >/dev/null 2>&1 || die "需要 dpkg-deb（dup 检查）"
[ -f "$ISO_SRC" ] || die "ISO_SRC 不存在：$ISO_SRC（本地上传官方 ISO，构建不联网）"
[ -f "$FRAGMENT" ] || die "缺少引导菜单片段：$FRAGMENT"
ls "$VPP_DEBS"/*.deb >/dev/null 2>&1 || die "VPP_DEBS 无 deb：$VPP_DEBS"
ls "$CLOSURE_DEBS"/*.deb >/dev/null 2>&1 || die "CLOSURE_DEBS 无 deb：$CLOSURE_DEBS"
if [ ! -f "$CLOSURE_DEBS/closure.done" ]; then
    log "警告：闭包未标记完成（closure.done 缺失）——清单可能不全，先跑补齐脚本"
fi
[ -f "$NFVIS_DEB" ] || die "nfvis deb 不存在：$NFVIS_DEB（make iso 会自动构建；直接调用请先 make deb VERSION=$VERSION 或设 NFVIS_DEB）"
DEB_VER="$(dpkg-deb -f "$NFVIS_DEB" Version)"
[ "$DEB_VER" = "$VERSION" ] || die "deb 版本（$DEB_VER）与 VERSION（$VERSION）不一致"

# 1) 组装 nfvis/ 暂存树（seed + deb 闭包）
rm -rf "$STAGING"
mkdir -p "$STAGING/nfvis/debs"
cp deploy/iso/user-data deploy/iso/meta-data "$STAGING/nfvis/"
HASH="$(openssl passwd -6 "$NFVIS_PASSWORD")"
sed -i "s|__NFVIS_PASSWORD_HASH__|$HASH|" "$STAGING/nfvis/user-data"
grep -q "$HASH" "$STAGING/nfvis/user-data" || die "口令哈希注入失败（sed 未生效？）"
cp "$CLOSURE_DEBS"/*.deb "$VPP_DEBS"/*.deb "$NFVIS_DEB" "$STAGING/nfvis/debs/"

# 2) 同包多版本检查：dpkg -i -R 同树内出现同名包两个版本会以最后解包者胜，
#    而「谁胜出」取决于文件处理顺序——不许赌，直接拦截。
declare -A SEEN=()
for f in "$STAGING/nfvis/debs"/*.deb; do
    p="$(dpkg-deb -f "$f" Package)"
    v="$(dpkg-deb -f "$f" Version)"
    if [ -n "${SEEN[$p]:-}" ]; then
        die "同包多版本：$p（${SEEN[$p]} / $v，$f）——请清理来源目录后重试"
    fi
    SEEN[$p]="$v"
done
DEB_COUNT=$(ls "$STAGING/nfvis/debs"/*.deb | wc -l)

# 3) 引导菜单：从 ISO 提取 grub.cfg，把 opt-in 菜单项插到 grub_platform 之前
#    （26.04 live-server 无 isolinux，BIOS/UEFI 共用这一份）。
#    缺省条目/超时不改——不选中 NFViS 项就还是原 Ubuntu 行为。
if ! xorriso -osirrox on -indev "$ISO_SRC" -extract /boot/grub/grub.cfg "$STAGING/grub.cfg" >/dev/null 2>&1; then
    die "从 ISO 提取 /boot/grub/grub.cfg 失败（ISO_SRC 不是 live-server ISO？）"
fi
grep -q "NFViS 自动安装" "$STAGING/grub.cfg" && die "ISO 已含 NFViS 菜单项——ISO_SRC 不是未加工的官方 ISO？"
LN="$(grep -n '^grub_platform' "$STAGING/grub.cfg" | head -1 | cut -d: -f1)"
if [ -n "$LN" ]; then
    head -n $((LN - 1)) "$STAGING/grub.cfg" > "$STAGING/grub.cfg.new"
    cat "$FRAGMENT" >> "$STAGING/grub.cfg.new"
    tail -n +"$LN" "$STAGING/grub.cfg" >> "$STAGING/grub.cfg.new"
    mv "$STAGING/grub.cfg.new" "$STAGING/grub.cfg"
else
    cat "$FRAGMENT" >> "$STAGING/grub.cfg"
fi
grep -q 'menuentry "Try or Install Ubuntu Server"' "$STAGING/grub.cfg" || die "注入后丢失原 Ubuntu 条目"
grep -q 'NFViS 自动安装' "$STAGING/grub.cfg" || die "菜单项注入失败"
grep -q '^set timeout=' "$STAGING/grub.cfg" || die "注入后丢失缺省超时（缺省引导行为被改？）"

# 4) remaster：原位复制 + 注入 nfvis/ 与新 grub.cfg，保留全部引导映像
mkdir -p "$(dirname "$ISO_OUT")"
XORRISO_LOG=build/iso-xorriso.log
if ! xorriso -indev "$ISO_SRC" -outdev "$ISO_OUT" \
        -boot_image any keep \
        -map "$STAGING/nfvis" /nfvis \
        -map "$STAGING/grub.cfg" /boot/grub/grub.cfg \
        > "$XORRISO_LOG" 2>&1; then
    tail -20 "$XORRISO_LOG"
    die "xorriso 失败（详见 $XORRISO_LOG）"
fi

# 5) 产物自检：目录注入在、grub.cfg 两条菜单都在、El Torito 引导记录还在
if ! xorriso -osirrox on -indev "$ISO_OUT" -extract /boot/grub/grub.cfg "$STAGING/verify.cfg" >/dev/null 2>&1; then
    die "输出 ISO 读不回 grub.cfg——产物不可用"
fi
grep -q 'menuentry "Try or Install Ubuntu Server"' "$STAGING/verify.cfg" || die "输出 ISO 缺原 Ubuntu 条目"
grep -q 'NFViS 自动安装' "$STAGING/verify.cfg" || die "输出 ISO 缺 NFViS 菜单项"
xorriso -indev "$ISO_OUT" -report_el_torito as_mkisofs > "$STAGING/torito.txt" 2>&1
grep -q -- '-b ' "$STAGING/torito.txt" || die "输出 ISO 缺 El Torito BIOS 引导记录"
grep -q 'efi.img' "$STAGING/torito.txt" || log "注意：未检出 EFI 引导映像（efi.img）——UEFI 引导需复核"

SIZE="$(du -h "$ISO_OUT" | cut -f1)"
log "已生成 $ISO_OUT（$SIZE，含 $DEB_COUNT 个 deb）"
log "装机验证（气隙）：qemu-system-x86_64 -enable-kvm -m 4096 -smp 4 \\"
log "    -drive file=test.qcow2,if=virtio -cdrom $ISO_OUT -boot once=d -nic none -display none -serial file:install-console.log"
