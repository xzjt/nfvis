#!/bin/bash
# 切 USTC 源（含 deb822 的 *.sources——上一版 sed 只盖了 *.list，源实际没切走）→
# 更新索引 → 补齐离线 deb 闭包（只下载缺失的）。
# 闭包范围裁剪（2026-09-19）：libvirt-daemon-driver-qemu 的 Depends 是候选组
#   qemu-system-x86 | qemu-system-x86-hwe | qemu-system | qemu-system-hwe
# apt 只装第一候选，而 apt-cache depends --recurse 把四个候选全展开——
# 元包 qemu-system/qemu-system-hwe 拖进 mips/ppc/riscv/sparc/s390x 全家族模拟器。
# 产品只要 x86_64，按下面的清单过滤掉（它们是叶子包，闭包内无反向依赖，裁剪安全）。
set -u

if grep -rq "mirrors.aliyun.com" /etc/apt/sources.list /etc/apt/sources.list.d/ 2>/dev/null; then
    sed -i.bak-ustc 's|mirrors\.aliyun\.com|mirrors.ustc.edu.cn|g' \
        /etc/apt/sources.list /etc/apt/sources.list.d/*.list /etc/apt/sources.list.d/*.sources 2>/dev/null || true
fi
echo "=== 当前源 ==="
grep -rhoE "https?://[^ ]*ubuntu[^ ]*" /etc/apt/sources.list /etc/apt/sources.list.d/ 2>/dev/null | sort -u | head -4

for pid in $(pgrep -f "apt-get [d]ownload"); do kill "$pid" 2>/dev/null; done
sleep 1

export DEBIAN_FRONTEND=noninteractive
apt-get update > /tmp/aptupdate-ustc.log 2>&1
echo "apt-get update rc=$?"
if ! grep -q "mirrors.ustc.edu.cn" /tmp/aptupdate-ustc.log; then
    echo "警告：apt update 未见 USTC 命中——检查源切换是否生效"
    tail -3 /tmp/aptupdate-ustc.log
fi

cd /root/isobuild/debs || exit 1

# 完整性预检：上次运行被中断会留下截断的 deb（dpkg-deb 读不了），删除后本循环重下，
# 否则「已有即跳过」会让坏文件永远滞留
for f in *.deb; do
    [ -e "$f" ] || continue
    dpkg-deb -f "$f" >/dev/null 2>&1 || { echo "删除损坏 deb: $f"; rm -f "$f"; }
done

PKGS=$(apt-cache depends --recurse --no-recommends --no-suggests --no-conflicts --no-breaks --no-replaces --no-enhances \
    docker.io libvirt-daemon-system libvirt-clients qemu-system-x86 qemu-utils cloud-image-utils 2>/dev/null \
    | grep -oE "^[a-z0-9][a-z0-9.+-]*" | sort -u)
NFVIS_EXCLUDE='^(qemu-system|qemu-system-hwe)$|^qemu-system-(mips|ppc|riscv|sparc|s390x|alpha|arm|aarch64|misc)(-hwe)?$'
PKGS=$(echo "$PKGS" | grep -Ev "$NFVIS_EXCLUDE")
# -hwe 双变体互斥：候选组把 <pkg> 与 <pkg>-hwe 同时展开，而二者 Conflicts/Replaces——
# 同树双变体会让 dpkg 解包一个拆掉另一个、依赖链 half-installed（round32 实测 libvirt 全链卡死）。
# 同名非 hwe 包也在闭包里时，剔除 hwe 变体（装机走非 hwe 缺省线）。
HWE_DUP=$(echo "$PKGS" | grep -- '-hwe$' | sed 's/-hwe$//' | while read -r p; do
    echo "$PKGS" | grep -qx "$p" && echo "${p}-hwe"
done)
PKGS=$(echo "$PKGS" | grep -vxF "$HWE_DUP")
echo "=== 闭包包数(裁剪后): $(echo "$PKGS" | wc -l) ==="
GOT=0
for p in $PKGS; do
    if ls "${p}_"*.deb >/dev/null 2>&1; then continue; fi
    apt-get download "$p" >> /root/isobuild/closure3.log 2>&1 || echo "下载失败: $p" >> /root/isobuild/closure2.err
    GOT=$((GOT+1))
done
# 清掉此前按未裁剪清单误下的模拟器包（叶子包，删了不影响闭包完整性）
rm -f qemu-system_*.deb qemu-system-hwe_*.deb \
      qemu-system-mips*.deb qemu-system-ppc*.deb qemu-system-riscv*.deb \
      qemu-system-sparc*.deb qemu-system-s390x*.deb qemu-system-misc*.deb \
      qemu-system-arm*.deb qemu-system-aarch64*.deb qemu-system-alpha*.deb
# 清掉互斥的 -hwe 双变体 deb（同名非 hwe 包在目录里时）
for f in *-hwe_*.deb; do
    [ -e "$f" ] || continue
    base="${f%%_*}"
    if ls "${base%-hwe}"_*.deb >/dev/null 2>&1; then
        echo "剔除互斥 hwe 变体: $f"
        rm -f "$f"
    fi
done
echo "本次补下 $GOT 个，累计 $(ls /root/isobuild/debs/*.deb 2>/dev/null | wc -l) 个"
echo DONE > /root/isobuild/debs/closure.done
