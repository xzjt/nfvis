#!/usr/bin/env bash
# NFViS 全功能 CLI 冒烟测试（**手动**，不在 CI/make check 中运行）
#
# 用途：逐条验证「契约 docs/NFViS-CLI命令树完整设计.md 声明的功能能否经 CLI 真正使用」。
# 由来：2026-09-14 全功能 CLI 测试（决策 #76）即用此套脚本，发现并修复 5 处
#       「契约已声明但经 CLI 不可用」的缺陷（详见 docs/evidence/v1-closeout-round8.txt）。
#
# 前置（缺一不可，均在 nfvis-vm 上）：
#   1) nfvisd 已起：/tmp/nfvisd -db /tmp/nfvis-cli.db -listen 127.0.0.1:18443 \
#        -init-admin-password "Admin@12345" -allow-plaintext
#   2) /tmp/nfvis-cli 为同版本构建产物；NFVIS_PASSWORD=Admin@12345
#   3) 引导镜像 /var/lib/nfvis/images/alpine.qcow2 存在（阶段 3 依赖）
#   4) Docker 本地有 alpine:3.20（阶段 3 依赖）
#   5) 隔离核池需留出至少 1 核给 VM（VPP 保留核会先从池中扣减），例如：
#        set resource-pools cpu isolated-cores 1-4 + set vpp cpu corelist-workers 2
#
# 用法：
#   bash contrib/scripts/cli-fulltest.sh            # 全部阶段
#   bash contrib/scripts/cli-fulltest.sh 1 5        # 只跑阶段 1 与 5
#
# 输出：逐条 ✓/✗ 与小结；原始输出见 /tmp/cli-test/full.log（可用 LOG= 覆盖）。
# 判定：行首 % / %% 或「校验失败」即失败。注意**环境受限项**（SR-IOV 无 PF/VF）
#       与**用例受限项**（重复设默认值触发「语句未产生配置变更」）也会显示 ✗，需人工判读。
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
export LOG=${LOG:-/tmp/cli-test/full.log}
# shellcheck source=cli-fulltest-lib.sh
source "$HERE/cli-fulltest-lib.sh"

phase_of() { # phase_of <phase1.sh> → 阶段号
  case "$1" in
    *phase1*) echo 1;;
    *phase2*) echo 2;;
    *phase3*) echo 3;;
    *phase4*) echo 4;;
    *phase5*) echo 5;;
    *phase6*) echo 6;;
    *) echo 0;;
  esac
}

want() { # want <n> <想跑的阶段...>：未指定则全跑
  local n="$1"; shift
  [ "$#" -eq 0 ] && return 0
  for a in "$@"; do [ "$a" = "$n" ] && return 0; done
  return 1
}

TOTAL_PASS=0; TOTAL_FAIL=0; TOTAL_EXP=0
for script in "$HERE"/cli-fulltest-phase*.sh; do
  n=$(phase_of "$script")
  [ "$n" = 0 ] && continue
  if ! want "$n" "$@"; then continue; fi
  out=$(bash "$script" 2>&1)
  printf '%s\n' "$out"
  # 阶段小结行形如「通过 P / 失败 F / 预期报错 E」
  line=$(printf '%s\n' "$out" | grep -E '^通过 [0-9]+ / 失败 [0-9]+ / 预期报错 [0-9]+$' | tail -1)
  p=$(printf '%s' "$line" | awk '{print $2}')
  f=$(printf '%s' "$line" | awk '{print $5}')
  e=$(printf '%s' "$line" | awk '{print $8}')
  TOTAL_PASS=$((TOTAL_PASS + ${p:-0}))
  TOTAL_FAIL=$((TOTAL_FAIL + ${f:-0}))
  TOTAL_EXP=$((TOTAL_EXP + ${e:-0}))
done

echo
echo "################ 全功能 CLI 冒烟合计 ################"
echo "通过 $TOTAL_PASS / 失败 $TOTAL_FAIL / 预期报错 $TOTAL_EXP"
echo "（「预期报错」= 环境受限或防呆守卫正确拒绝；失败项需人工判读，见脚本头部说明）"
