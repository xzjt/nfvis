#!/usr/bin/env bash
# NFViS 离线一键安装包（自解压 .run）生成器。
#
# 产物 = 头部脚本 + gzip 载荷（tar），载荷内容：
#   install.sh         安装器（源在 deploy/offline/install.sh）
#   VERSION            版本号
#   MANIFEST.tsv       每个 deb 的包名/版本/架构/sha256/文件名（可审计溯源）
#   BUILD-INFO.txt     构建信息（时间锚、deb 组别、来源快照、闭包校验结论）
#   SHA256SUMS         载荷完整性清单（安装前逐文件校验）
#   debs/*.deb         产品包 + VPP + 全部依赖闭包
#   debs/Packages      本地 apt 源的索引（扁平源，安装时只指向它）
#
# 用法：
#   bash contrib/scripts/build-offline-run.sh --debs DIR --version 1.1.47 --output build/nfvis-v1.1.47.run
# 选项：
#   --debs DIR        deb 目录（用 offline-closure.sh fetch 抓齐的闭包；必须含 nfvis 包）
#   --version X.Y.Z   版本号（必须与 debs 里 nfvis 包的版本一致）
#   --output FILE     产物路径
#   --epoch N         时间锚（缺省取 HEAD 提交时间；决定包内 mtime 与头部构建日期）
#   --index-only DIR  只生成载荷目录（不打包）——供自校准脚本复用同一套索引逻辑
#
# 可复现：同一批 deb + 同一 VERSION + 同一 --epoch ⇒ 逐字节相同的 .run
#   （tar 成员按名排序、mtime 统一、属主归零；gzip 用 -n 去掉时间戳与文件名）。
set -u

die() { printf '错误：%s\n' "$*" >&2; exit 2; }
have() { command -v "$1" >/dev/null 2>&1; }

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
DEBS=""; VERSION=""; OUT=""; INDEX_ONLY=""
EPOCH=$(git -C "$ROOT" log -1 --format=%ct 2>/dev/null || echo 0)

while [ $# -gt 0 ]; do
    case "$1" in
        --debs) shift; DEBS=${1:-} ;;
        --version) shift; VERSION=${1:-} ;;
        --output) shift; OUT=${1:-} ;;
        --epoch) shift; EPOCH=${1:-} ;;
        --index-only) shift; INDEX_ONLY=${1:-} ;;
        *) die "未知参数：$1" ;;
    esac
    shift
done

[ -n "$DEBS" ] || die "需要 --debs DIR"
[ -d "$DEBS" ] || die "deb 目录不存在：$DEBS"
[ -n "$VERSION" ] || die "需要 --version X.Y.Z"
[ -n "$OUT" ] || [ -n "$INDEX_ONLY" ] || die "需要 --output FILE（或 --index-only DIR）"
for t in dpkg-deb tar gzip sha256sum md5sum awk stat; do have "$t" || die "缺少命令：$t"; done

INSTALL_SH="$ROOT/deploy/offline/install.sh"
[ -f "$INSTALL_SH" ] || die "找不到安装器：$INSTALL_SH"
bash -n "$INSTALL_SH" || die "安装器语法检查失败：$INSTALL_SH"

NDEB=$(find "$DEBS" -maxdepth 1 -name '*.deb' | wc -l)
[ "$NDEB" -gt 0 ] || die "目录里没有 deb：$DEBS"

# 产品包必须在场，且版本与 --version 一致（否则装上的是别的版本，产物名会骗人）
NFVIS_DEB=$(find "$DEBS" -maxdepth 1 -name 'nfvis_*.deb' | sort | head -1)
[ -n "$NFVIS_DEB" ] || die "deb 目录里没有 nfvis_*.deb（先跑 make deb）"
NFVIS_VER=$(dpkg-deb -f "$NFVIS_DEB" Version)
[ "$NFVIS_VER" = "$VERSION" ] ||
    die "版本不一致：nfvis 包是 $NFVIS_VER，--version 给的是 $VERSION"

# ---------- 本地 apt 源索引（扁平源；只用 dpkg-deb，不需要 dpkg-dev） ----------
gen_packages() { # $1=deb 目录 $2=输出文件
    local d name
    : > "$2"
    for d in "$1"/*.deb; do
        [ -e "$d" ] || continue
        name=$(dpkg-deb -f "$d" Package)
        [ -n "$name" ] || die "读不出包名：$d"
        printf 'Package: %s\n' "$name"
        printf 'Version: %s\n' "$(dpkg-deb -f "$d" Version)"
        printf 'Architecture: %s\n' "$(dpkg-deb -f "$d" Architecture)"
        local f v
        for f in Section Priority Essential Multi-Arch Pre-Depends Depends Provides \
            Conflicts Breaks Replaces Recommends Suggests; do
            v=$(dpkg-deb -f "$d" "$f" 2>/dev/null || true)
            [ -n "$v" ] && printf '%s: %s\n' "$f" "$v"
        done
        printf 'Filename: ./%s\n' "$(basename "$d")"
        printf 'Size: %s\n' "$(stat -c %s "$d")"
        printf 'SHA256: %s\n' "$(sha256sum "$d" | cut -d' ' -f1)"
        printf 'MD5sum: %s\n' "$(md5sum "$d" | cut -d' ' -f1)"
        # 只取短描述首行：apt 对多行 Description 的续行缩进有严格要求，
        # 而这一栏对本地源无实际作用，保持单行最稳。
        printf 'Description: %s\n\n' "$(dpkg-deb -f "$d" Description 2>/dev/null | head -1 || echo "$name")"
    done
}

stage_payload() { # $1=目标载荷目录
    local P=$1
    rm -rf "$P"
    mkdir -p "$P/debs"
    local d
    for d in "$DEBS"/*.deb; do
        [ -e "$d" ] || continue
        cp -p "$d" "$P/debs/" 2>/dev/null || cp "$d" "$P/debs/"
    done
    gen_packages "$P/debs" "$P/debs/Packages"
    printf '%s\n' "$VERSION" > "$P/VERSION"
    install -m 0755 "$INSTALL_SH" "$P/install.sh"

    # 可审计清单：包名/版本/架构/sha256/文件名
    {
        printf '# 名\t版本\t架构\tsha256\t文件\n'
        local n v a
        for d in "$P"/debs/*.deb; do
            n=$(dpkg-deb -f "$d" Package)
            v=$(dpkg-deb -f "$d" Version)
            a=$(dpkg-deb -f "$d" Architecture)
            printf '%s\t%s\t%s\t%s\t%s\n' "$n" "$v" "$a" "$(sha256sum "$d" | cut -d' ' -f1)" "$(basename "$d")"
        done
    } > "$P/MANIFEST.tsv"

    # 构建信息（人读）
    BUILT=$(date -u -d "@$EPOCH" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo "epoch=$EPOCH")
    {
        printf 'nfvis 离线安装包\n'
        printf '  版本      %s\n' "$VERSION"
        printf '  构建时间锚 %s（SOURCE_DATE_EPOCH=%s）\n' "$BUILT" "$EPOCH"
        printf '  deb 数    %s（合计 %s）\n' "$(find "$P/debs" -name '*.deb' | wc -l)" "$(du -sh "$P/debs" | cut -f1)"
        printf '  包清单    见 MANIFEST.tsv\n'
        if [ -f "$DEBS/FETCH-SUMMARY.txt" ]; then
            printf '  来源      \n'
            sed 's/^/            /' "$DEBS/FETCH-SUMMARY.txt"
        fi
    } > "$P/BUILD-INFO.txt"

    (cd "$P" && find . -type f ! -name SHA256SUMS -printf '%P\n' | sort | xargs sha256sum > SHA256SUMS)
}

if [ -n "$INDEX_ONLY" ]; then
    stage_payload "$INDEX_ONLY"
    printf '载荷目录已生成：%s（deb %s 个）\n' "$INDEX_ONLY" "$NDEB"
    exit 0
fi

WORK=$(mktemp -d) || die "无法创建临时目录"
trap 'rm -rf "$WORK"' EXIT
PAYLOAD_DIR="$WORK/payload"
stage_payload "$PAYLOAD_DIR"

# 载荷打包：确定性（排序、固定 mtime/属主、gzip -n 不带时间戳与文件名）
( cd "$PAYLOAD_DIR" && tar --sort=name --mtime="@$EPOCH" --owner=0 --group=0 --numeric-owner \
    -cf - . | gzip -9n ) > "$WORK/payload.tar.gz" || die "载荷打包失败"

mkdir -p "$(dirname "$OUT")"
HDR="$WORK/header.sh"
cat > "$HDR" <<'HEADER'
#!/bin/sh
# nfvis 离线安装包（自解压）：内含产品包与其全部依赖 deb，无需联网、无需构建工具。
#   版本 @VERSION@ · 构建 @BUILT@ · 内含 @NDEB@ 个 deb（@SIZE@）
#
# 运行前请确认：本机是干净的 Ubuntu 26.04 amd64，且以 root 运行。
# 安装过程只使用包内自带的 deb（临时建一个指向解包目录的本地 apt 源），
# 不改系统 apt 源、不访问网络、不碰业务网卡；装完会做一轮带独立事实源的自检。
set -eu

RUN_VERSION='@VERSION@'
MARK='__NFVIS_PAYLOAD_BELOW__'

usage() {
    cat <<'USAGE'
用法：sh nfvis-v@VERSION@.run [选项]
  -y, --yes               跳过确认（自动化；非交互场景必给）
      --admin-password P  预置 admin 口令（至少 8 个字符，仅限字母数字与 @._+=-）
      --no-start          只安装，不起服务
      --verify            只体检（假定已安装；不安装）
      --keep              保留解包目录（排查用）
      --extract DIR       只把载荷解到 DIR，不安装
  -h, --help              显示本帮助
      --version           显示版本
USAGE
}

case "${1:-}" in
    -h | --help) usage; exit 0 ;;
    --version) printf 'nfvis offline installer %s\n' "$RUN_VERSION"; exit 0 ;;
esac

command -v tar >/dev/null 2>&1 || { echo "错误：缺少 tar，无法解包" >&2; exit 2; }
[ "$(id -u)" = 0 ] || { echo "错误：请以 root 运行（sudo sh $0 ...）" >&2; exit 2; }

LINE=$(awk -v m="$MARK" '$0 == m { print NR + 1; exit }' "$0")
[ -n "${LINE:-}" ] || { echo "错误：载荷定位失败（文件被截断或改动过？）" >&2; exit 2; }

EXTRACT_TO=""
if [ "${1:-}" = "--extract" ]; then
    EXTRACT_TO="${2:-}"
    [ -n "$EXTRACT_TO" ] || { echo "错误：--extract 需要目录" >&2; exit 2; }
    mkdir -p "$EXTRACT_TO"
    tail -n +"$LINE" "$0" | tar -xzf - -C "$EXTRACT_TO" || { echo "错误：解包失败" >&2; exit 2; }
    printf '载荷已解到：%s\n' "$EXTRACT_TO"
    exit 0
fi

STAGE=$(mktemp -d /var/tmp/nfvis-run.XXXXXX) || { echo "错误：无法创建临时目录" >&2; exit 2; }
KEEP=0
for a in "$@"; do
    case "$a" in --keep) KEEP=1 ;; esac
done
if [ "$KEEP" != 1 ]; then trap 'rm -rf "$STAGE"' EXIT INT TERM; fi

tail -n +"$LINE" "$0" | tar -xzf - -C "$STAGE" || { echo "错误：解包失败" >&2; exit 2; }
set +e
bash "$STAGE/install.sh" --payload "$STAGE" "$@"
rc=$?
set -e
if [ "$KEEP" = 1 ]; then printf '解包目录保留：%s\n' "$STAGE"; fi
exit "$rc"
HEADER

BUILT=$(date -u -d "@$EPOCH" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo "epoch=$EPOCH")
SIZE=$(du -sh "$WORK/payload.tar.gz" | cut -f1)
sed -i "s|@VERSION@|$VERSION|g; s|@BUILT@|$BUILT|g; s|@NDEB@|$NDEB|g; s|@SIZE@|$SIZE|g" "$HDR"
sh -n "$HDR" || die "头部脚本语法检查失败"

printf '%s\n' "$MARK" >> "$HDR"
cat "$HDR" "$WORK/payload.tar.gz" > "$OUT"
chmod +x "$OUT"
sha256sum "$OUT" | awk '{print $1}' > "$OUT.sha256"

printf '已生成 %s\n' "$OUT"
printf '  版本 %s · deb %s 个 · 大小 %s\n' "$VERSION" "$NDEB" "$(du -h "$OUT" | cut -f1)"
printf '  sha256 %s\n' "$(cat "$OUT.sha256")"
