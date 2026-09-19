#!/usr/bin/env bash
# NFViS 安装 ISO 构建（决策 #110）：Ubuntu Server live ISO remaster + autoinstall + 离线底座闭包。
#
# 用法（Linux 构建机 / nfvis-vm）：
#   make iso VERSION=1.1.20                      # 先构建 nfvis deb 再 remaster
#   NFVIS_DEB=path/to/nfvis_x.y.z_amd64.deb bash contrib/iso/build-iso.sh  # 复用已有 deb
#
# 输入（环境变量，缺省值即验证机 nfvis-vm 的现场路径；构建不联网）：
#   ISO_SRC        官方 live-server ISO（须已对官方 SHA256SUMS 校验，且是未加工原版）
#   VPP_DEBS       VPP deb 目录（非 Ubuntu 源，必须随包）
#   CLOSURE_DEBS   离线闭包目录（docker/libvirt/qemu/cloud-image-utils 递归闭包，
#                  应含补齐脚本落下的 closure.done 标记）
#   NFVIS_DEB      nfvis deb（缺省 build/nfvis_VERSION_ARCH.deb）
#   NFVIS_PASSWORD 装机后 OS 用户 nfvis 的口令（构建时生成 crypt 哈希注入 seed）
#   ISO_OUT        输出路径（缺省 build/nfvis_VERSION_ARCH.iso）
#
# remaster 机制：**按原 ISO 自报配方全树重建**——先 `-report_el_torito as_mkisofs`
# 取回官方引导配方（MBR/BIOS El Torito/EFI 取自附加分区字节区间，均引用 ISO_SRC），
# 再全树提取 + 注入 nfvis/ 与新 grub.cfg，最后 `xorriso -as mkisofs` 重建。
# 不能用 indev→outdev `-boot_image any keep` 原位复制：26.04 的 EFI El Torito
# 指向"隐藏映像"（附加分区区间，不在 ISO 文件树里），keep 无法重新编址，
# 实测产物 `-report_el_torito` 报 SORRY（boot image #2 非 ISO 文件）。
# 产物结构：/nfvis/{user-data,meta-data,debs/} + boot/grub/grub.cfg 追加 opt-in
# 菜单项「NFViS 自动安装（将清空所选磁盘）」，缺省引导条目仍是原 Ubuntu（不选中即不清盘）。
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
# 闭包裁剪（与 contrib/iso/ustc-closure.sh 同清单）：libvirt-daemon-driver-qemu 的
# Depends 候选组把 qemu-system/qemu-system-hwe 元包及其拖进来的全家族模拟器
# 展开进了递归闭包——apt 只会装第一候选（qemu-system-x86），这些包产品用不上，
# 且 dpkg -i -R 会全量安装；它们是叶子包，剔除不破坏其余包的依赖解析。
EXCLUDE='^(qemu-system|qemu-system-hwe)$|^qemu-system-(mips|ppc|riscv|sparc|s390x|alpha|arm|aarch64|misc)(-hwe)?$'

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

rm -rf "$STAGING"
mkdir -p "$STAGING"

# 1) 引导配方：原 ISO 必须是未加工原版（自报配方可读且含 BIOS+EFI 两条引导记录）
if ! xorriso -indev "$ISO_SRC" -report_el_torito as_mkisofs \
        > "$STAGING/mkisofs-recipe.txt" 2> "$STAGING/mkisofs-recipe.err"; then
    die "ISO_SRC 读取引导配方失败（非官方原版 ISO？）——见 $STAGING/mkisofs-recipe.err"
fi
grep -q -- '-b ' "$STAGING/mkisofs-recipe.txt" || die "引导配方缺 BIOS 记录（-b）"
grep -q -- '-e ' "$STAGING/mkisofs-recipe.txt" || die "引导配方缺 EFI 记录（-e）——ISO_SRC 结构异常"

# 2) 全树提取（含注入所需的 boot/grub/grub.cfg）；ISO 提取出的文件常为只读，补写权限
log "提取原 ISO 全树…"
if ! xorriso -osirrox on -indev "$ISO_SRC" -extract / "$STAGING/tree" \
        > "$STAGING/extract.log" 2>&1; then
    tail -5 "$STAGING/extract.log"
    die "全树提取失败"
fi
chmod -R u+w "$STAGING/tree"

# 3) 注入 nfvis/（seed + deb 闭包）。先全量采集包名——-hwe 双变体互斥判断需要全集：
#    <pkg> 与 <pkg>-hwe 同时在树时（候选组展开所致）二者 Conflicts/Replaces，
#    dpkg 解包一个拆掉另一个、依赖链 half-installed（round32 实测 libvirt 全链卡死）；
#    非管理面装机走非 hwe 缺省线，故有同名非 hwe 时剔除 hwe 变体。
mkdir -p "$STAGING/tree/nfvis/debs"
cp deploy/iso/user-data deploy/iso/meta-data "$STAGING/tree/nfvis/"
HASH="$(openssl passwd -6 "$NFVIS_PASSWORD")"
sed -i "s|__NFVIS_PASSWORD_HASH__|$HASH|" "$STAGING/tree/nfvis/user-data"
grep -q "$HASH" "$STAGING/tree/nfvis/user-data" || die "口令哈希注入失败（sed 未生效？）"
declare -A PKGNAMES=()
for src in "$CLOSURE_DEBS"/*.deb "$VPP_DEBS"/*.deb "$NFVIS_DEB"; do
    if ! pkg="$(dpkg-deb -f "$src" Package 2>/dev/null)"; then
        die "闭包含损坏 deb（多半是下载中断残留，删掉后重跑闭包脚本）：$src"
    fi
    PKGNAMES[$pkg]=1
done
for src in "$CLOSURE_DEBS"/*.deb "$VPP_DEBS"/*.deb "$NFVIS_DEB"; do
    base="$(basename "$src")"
    case "$base" in
        # 防御性裁剪：即使闭包目录未重跑补齐脚本，也不让模拟器包混进 ISO
        qemu-system_*|qemu-system-hwe_*|qemu-system-mips*|qemu-system-ppc*|qemu-system-riscv*|qemu-system-sparc*|qemu-system-s390x*|qemu-system-misc*) continue ;;
    esac
    pkg="$(dpkg-deb -f "$src" Package 2>/dev/null)" || die "损坏 deb：$src"
    if echo "$pkg" | grep -qE "$EXCLUDE"; then continue; fi
    if [ "${pkg%-hwe}" != "$pkg" ] && [ -n "${PKGNAMES[${pkg%-hwe}]:-}" ]; then continue; fi
    cp "$src" "$STAGING/tree/nfvis/debs/"
done

# 4) 同包多版本检查：dpkg -i -R 同树内出现同名包两个版本会以最后解包者胜，
#    而「谁胜出」取决于文件处理顺序——不许赌，直接拦截。
declare -A SEEN=()
for f in "$STAGING/tree/nfvis/debs"/*.deb; do
    p="$(dpkg-deb -f "$f" Package)"
    v="$(dpkg-deb -f "$f" Version)"
    if [ -n "${SEEN[$p]:-}" ]; then
        die "同包多版本：$p（${SEEN[$p]} / $v，$f）——请清理来源目录后重试"
    fi
    SEEN[$p]="$v"
done
DEB_COUNT=$(ls "$STAGING/tree/nfvis/debs"/*.deb | wc -l)

# 3b) 生成本地 file 仓库索引：装机期 apt 从这个仓库解析安装（dpkg -i -R 直灌
#     会把候选组展开出的互斥备选实现族全塞进去——cron/bcron/systemd-cron、
#     dnsmasq-base/-lua、libvirt -hwe 双变体……round32 两次装机实证必翻车；
#     apt 只挑每组可满足候选，与正常安装语义一致）
(
    cd "$STAGING/tree/nfvis/debs" || exit 1
    dpkg-scanpackages --arch "$ARCH" . > Packages
    gzip -9n -c Packages > Packages.gz
    apt-ftparchive release . > Release
) || die "本地仓库索引生成失败（需 dpkg-scanpackages / apt-ftparchive）"

# 5) 引导菜单：opt-in 菜单项插到 grub_platform 之前（26.04 live-server 无 isolinux，
#    BIOS/UEFI 共用这一份）。缺省条目/超时不改——不选中 NFViS 项就还是原 Ubuntu 行为。
GRUB_CFG="$STAGING/tree/boot/grub/grub.cfg"
[ -f "$GRUB_CFG" ] || die "树内缺 boot/grub/grub.cfg"
grep -q "NFViS 自动安装" "$GRUB_CFG" && die "ISO 已含 NFViS 菜单项——ISO_SRC 不是未加工的官方 ISO？"
LN="$(grep -n '^grub_platform' "$GRUB_CFG" | head -1 | cut -d: -f1)"
if [ -n "$LN" ]; then
    head -n $((LN - 1)) "$GRUB_CFG" > "$GRUB_CFG.new"
    cat "$FRAGMENT" >> "$GRUB_CFG.new"
    tail -n +"$LN" "$GRUB_CFG" >> "$GRUB_CFG.new"
    mv "$GRUB_CFG.new" "$GRUB_CFG"
else
    cat "$FRAGMENT" >> "$GRUB_CFG"
fi
grep -q 'menuentry "Try or Install Ubuntu Server"' "$GRUB_CFG" || die "注入后丢失原 Ubuntu 条目"
grep -q 'NFViS 自动安装' "$GRUB_CFG" || die "菜单项注入失败"
grep -q '^set timeout=' "$GRUB_CFG" || die "注入后丢失缺省超时（缺省引导行为被改？）"

# 6) 重建：官方配方 + 注入后的树。配方内 --interval 引用 ISO_SRC 字节区间（MBR/EFI），
#    逐字回放即可；-o 与树路径由本脚本追加。
#    配方值带空格（如卷 ID）且已由 xorriso 用单引号包裹——必须经 eval 展开，
#    裸 $(cat) 分词会把 'Ubuntu-Server 26.04.1 LTS amd64' 拆成四个参数（实测踩过）；
#    引号内 $(cat) 又会保留换行、被 eval 当多条命令——先并成一行。
mkdir -p "$(dirname "$ISO_OUT")"
rm -f "$ISO_OUT"
XORRISO_LOG=build/iso-xorriso.log
eval "set -- $(tr '\n' ' ' < "$STAGING/mkisofs-recipe.txt")"
if ! xorriso -as mkisofs "$@" \
        -o "$ISO_OUT" "$STAGING/tree" > "$XORRISO_LOG" 2>&1; then
    tail -20 "$XORRISO_LOG"
    die "xorriso 重建失败（详见 $XORRISO_LOG）"
fi

# 7) 产物自检：引导配方完整（BIOS+EFI）、grub.cfg 两条菜单都在、nfvis/ 注入在
if ! xorriso -indev "$ISO_OUT" -report_el_torito as_mkisofs \
        > "$STAGING/verify-torito.txt" 2>"$STAGING/verify-torito.err"; then
    tail -5 "$STAGING/verify-torito.err"
    die "产物引导配置不完整（as_mkisofs 报错）——EFI/BIOS 引导需复核"
fi
grep -q -- '-b ' "$STAGING/verify-torito.txt" || die "产物缺 BIOS 引导记录（-b）"
grep -q -- '-e ' "$STAGING/verify-torito.txt" || die "产物缺 EFI 引导记录（-e）"
if ! xorriso -osirrox on -indev "$ISO_OUT" -extract /boot/grub/grub.cfg "$STAGING/verify.cfg" >/dev/null 2>&1; then
    die "输出 ISO 读不回 grub.cfg——产物不可用"
fi
grep -q 'menuentry "Try or Install Ubuntu Server"' "$STAGING/verify.cfg" || die "输出 ISO 缺原 Ubuntu 条目"
grep -q 'NFViS 自动安装' "$STAGING/verify.cfg" || die "输出 ISO 缺 NFViS 菜单项"
xorriso -indev "$ISO_OUT" -find /nfvis -name user-data 2>/dev/null | grep -q user-data || die "输出 ISO 缺 nfvis/user-data"

SIZE="$(du -h "$ISO_OUT" | cut -f1)"
log "已生成 $ISO_OUT（$SIZE，含 $DEB_COUNT 个 deb）"
log "装机验证（气隙）：qemu-system-x86_64 -enable-kvm -m 4096 -smp 4 \\"
log "    -drive file=test.qcow2,if=virtio -cdrom $ISO_OUT -boot once=d -nic none -display none -serial file:install-console.log"
